package raknet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

const (
	// bitFlagValid is set for every valid datagram. It is used to identify packets that are datagrams.
	bitFlagValid = 0x80
	// bitFlagACK is set for every ACK packet.
	bitFlagACK = 0x40
	// bitFlagNACK is set for every NACK packet.
	bitFlagNACK = 0x20
)

const (
	idConnectedPing = 0x00
	idConnectedPong = 0x03

	idConnectionRequest = 0x09
	idConnectionRequestAccepted = 0x10
	idNewIncomingConnection = 0x13
	idDisconnectNotification = 0x15
)

const (
	// reliabilityUnreliable means that the packet sent could arrive out of order, be duplicated, or just not
	// arrive at all. It is usually used for high frequency packets of which the order does not matter.
	reliabilityUnreliable byte = iota
	// reliabilityUnreliableSequenced means that the packet sent could be duplicated or not arrive at all, but
	// ensures that it is always handled in the right order.
	reliabilityUnreliableSequenced
	// reliabilityReliable means that the packet sent could not arrive, or arrive out of order, but ensures
	// that the packet is not duplicated.
	reliabilityReliable
	// reliabilityReliableOrdered means that every packet sent arrives, arrives in the right order and is not
	// duplicated.
	reliabilityReliableOrdered
	// reliabilityReliableSequenced means that the packet sent could not arrive, but ensures that the packet
	// will be in the right order and not be duplicated.
	reliabilityReliableSequenced
	// reliabilityUnreliableWithAck means that the packet sent could arrive out of order, be duplicated or
	// just not arrive at all. The client will send an acknowledgement if it got the packet.
	reliabilityUnreliableWithAck
	// reliabilityReliableWithAck means that every packet sent arrives, arrives in the right order and is not
	// duplicated. The client will send an acknowledgement if it got the packet.
	reliabilityReliableWithAck
	// reliabilityReliableOrderedWithAck means that the packet sent could not arrive, but ensures that the
	// packet will be in the right order and not be duplicated. The client will send an acknowledgement if it
	// got the packet.
	reliabilityReliableOrderedWithAck

	// splitFlag is set in the header if the packet was split. If so, the encapsulation contains additional
	// data about the fragment.
	splitFlag byte = 0x10
)

type connectedPing struct {
	PingTimestamp int64
}

type connectedPong struct {
	PingTimestamp int64
	PongTimestamp int64
}

type connectionRequest struct {
	ClientGUID int64
	RequestTimestamp int64
	Secure bool
}

type connectionRequestAccepted struct {
	// ClientAddress
	RequestTimestamp int64
	AcceptedTimestamp int64
	// 20 system addresses
}

type newIncomingConnection connectionRequestAccepted

type packet struct {
	reliability byte

	content    []byte
	messageIndex uint32
	sequenceIndex uint32
	orderIndex uint32

	split      bool
	splitCount uint32
	splitIndex uint32
	splitID    uint16
}

func (packet *packet) write(b *bytes.Buffer) error {
	header := packet.reliability << 5
	if packet.split {
		header |= splitFlag
	}
	if err := b.WriteByte(header); err != nil {
		return fmt.Errorf("error writing packet header: %v", err)
	}
	if err := binary.Write(b, binary.BigEndian, uint16(len(packet.content)) << 3); err != nil {
		return fmt.Errorf("error writing packet content length: %v", err)
	}
	if packet.reliable() {
		if err := writeUint24(b, packet.messageIndex); err != nil {
			return fmt.Errorf("error writing packet message index: %v", err)
		}
	}
	if packet.sequenced() {
		if err := writeUint24(b, packet.sequenceIndex); err != nil {
			return fmt.Errorf("error writing packet sequence index: %v", err)
		}
	}
	if packet.sequencedOrOrdered() {
		if err := writeUint24(b, packet.orderIndex); err != nil {
			return fmt.Errorf("error writing packet order index: %v", err)
		}
		// Order channel, we don't care about this.
		_ = b.WriteByte(0)
	}
	if packet.split {
		if err := binary.Write(b, binary.BigEndian, packet.splitCount); err != nil {
			return fmt.Errorf("error writing packet split count: %v", err)
		}
		if err := binary.Write(b, binary.BigEndian, packet.splitID); err != nil {
			return fmt.Errorf("error writing packet split ID: %v", err)
		}
		if err := binary.Write(b, binary.BigEndian, packet.splitIndex); err != nil {
			return fmt.Errorf("error writing packet split index: %v", err)
		}
	}
	if _, err := b.Write(packet.content); err != nil {
		return fmt.Errorf("error writing packet content: %v", err)
	}
	return nil
}

func (packet *packet) read(b *bytes.Buffer) error {
	header, err := b.ReadByte()
	if err != nil {
		return fmt.Errorf("error reading packet header: %v", err)
	}
	packet.split = (header & splitFlag) != 0
	packet.reliability = (header & 224) >> 5
	var packetLength uint16
	if err := binary.Read(b, binary.BigEndian, &packetLength); err != nil {
		return fmt.Errorf("error reading packet length: %v", err)
	}
	packetLength >>= 3
	if packetLength == 0 {
		return fmt.Errorf("invalid packet length: cannot be 0")
	}

	if packet.reliable() {
		packet.messageIndex, err = readUint24(b)
		if err != nil {
			return fmt.Errorf("error reading packet message index: %v", err)
		}
	}

	if packet.sequenced() {
		packet.sequenceIndex, err = readUint24(b)
		if err != nil {
			return fmt.Errorf("error reading packet sequence index: %v", err)
		}
	}

	if packet.sequencedOrOrdered() {
		packet.orderIndex, err = readUint24(b)
		if err != nil {
			return fmt.Errorf("error reading packet order index: %v", err)
		}
		// Order channel (byte), we don't care about this.
		b.Next(1)
	}

	if packet.split {
		if err := binary.Read(b, binary.BigEndian, &packet.splitCount); err != nil {
			return fmt.Errorf("error reading packet split count: %v", err)
		}
		if err := binary.Read(b, binary.BigEndian, &packet.splitID); err != nil {
			return fmt.Errorf("error reading packet split ID: %v", err)
		}
		if err := binary.Read(b, binary.BigEndian, &packet.splitIndex); err != nil {
			return fmt.Errorf("error reading packet split index: %v", err)
		}
	}

	packet.content = make([]byte, packetLength)
	if _, err := b.Read(packet.content); err != nil {
		return fmt.Errorf("error reading packet content: %v", err)
	}
	return nil
}

func (packet *packet) reliable() bool {
	switch packet.reliability {
	case reliabilityReliable,
		reliabilityReliableOrdered,
		reliabilityReliableSequenced,
		reliabilityReliableWithAck,
		reliabilityReliableOrderedWithAck:
		return true
	}
	return false
}

func (packet *packet) sequencedOrOrdered() bool {
	switch packet.reliability {
	case reliabilityUnreliableSequenced,
		reliabilityReliableOrdered,
		reliabilityReliableSequenced,
		reliabilityReliableOrderedWithAck:
		return true
	}
	return false
}

func (packet *packet) sequenced() bool {
	switch packet.reliability {
	case reliabilityUnreliableSequenced,
		reliabilityReliableSequenced:
		return true
	}
	return false
}

const (
	// PacketRange indicates a range of packets, followed by the first and the last packet in the range.
	PacketRange = iota
	// PacketSingle indicates a single packet, followed by its sequence number.
	PacketSingle
)

// acknowledgement is an acknowledgement packet that may either be an ACK or a NACK, depending on the purpose
// that it is sent with.
type acknowledgement struct {
	packets []uint32
}

// write writes an acknowledgement packet and returns an error if not successful.
func (ack *acknowledgement) write(b *bytes.Buffer) error {
	if len(ack.packets) == 0 {
		return b.WriteByte(0)
	}
	buffer := bytes.NewBuffer(nil)
	// Sort packets before encoding to ensure packets are encoded correctly.
	sort.Slice(ack.packets, func(i, j int) bool {
		return ack.packets[i] < ack.packets[j]
	})

	var firstPacketInRange uint32
	var lastPacketInRange uint32
	var recordCount int16 = 1

	for index, packet := range ack.packets {
		if index == 0 {
			// The first packet, set the first and last packet to it.
			firstPacketInRange = packet
			lastPacketInRange = packet
			continue
		}
		if packet == lastPacketInRange+1 {
			// Packet is still part of the current range, as it's sequenced properly with the last packet.
			// Set the last packet in range to the packet and continue to the next packet.
			lastPacketInRange = packet
			continue
		} else {
			// We got to the end of a range/single packet. We need to write those down now.
			if firstPacketInRange == lastPacketInRange {
				// First packet equals last packet, so we have a single packet record. Write down the packet,
				// and set the first and last packet to the current packet.
				if err := buffer.WriteByte(PacketSingle); err != nil {
					return err
				}
				if err := writeUint24(buffer, firstPacketInRange); err != nil {
					return err
				}

				firstPacketInRange = packet
				lastPacketInRange = packet
			} else {
				// There's a gap between the first and last packet, so we have a range of packets. Write the
				// first and last packet of the range and set both to the current packet.
				if err := buffer.WriteByte(PacketRange); err != nil {
					return err
				}
				if err := writeUint24(buffer, firstPacketInRange); err != nil {
					return err
				}
				if err := writeUint24(buffer, lastPacketInRange); err != nil {
					return err
				}

				firstPacketInRange = packet
				lastPacketInRange = packet
			}
			// Keep track of the amount of records as we need to write that first.
			recordCount++
		}
	}

	// Make sure the last single packet/range is written, as we always need to know one packet ahead to know
	// how we should write the current.
	if firstPacketInRange == lastPacketInRange {
		if err := buffer.WriteByte(PacketSingle); err != nil {
			return err
		}
		if err := writeUint24(buffer, firstPacketInRange); err != nil {
			return err
		}
	} else {
		if err := buffer.WriteByte(PacketRange); err != nil {
			return err
		}
		if err := writeUint24(buffer, firstPacketInRange); err != nil {
			return err
		}
		if err := writeUint24(buffer, lastPacketInRange); err != nil {
			return err
		}
	}
	if err := binary.Write(b, binary.BigEndian, recordCount); err != nil {
		return err
	}
	if _, err := b.Write(buffer.Bytes()); err != nil {
		return err
	}
	return nil
}

// read reads an acknowledgement packet and returns an error if not successful.
func (ack *acknowledgement) read(b *bytes.Buffer) error {
	var recordCount int16
	if err := binary.Read(b, binary.BigEndian, &recordCount); err != nil {
		return err
	}
	count := 0
	for i := int16(0); i < recordCount; i++ {
		recordType, err := b.ReadByte()
		if err != nil {
			return err
		}
		switch recordType{
		case PacketRange:
			start, err := readUint24(b)
			if err != nil {
				return err
			}
			end, err := readUint24(b)
			if err != nil {
				return err
			}
			for pack := start; pack <= end; pack++ {
				ack.packets = append(ack.packets, pack)
				count++
			}
		case PacketSingle:
			packet, err := readUint24(b)
			if err != nil {
				return err
			}
			ack.packets = append(ack.packets, packet)
			count++
		}
	}
	return nil
}