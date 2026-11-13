package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

func main() {
	conn, err := net.DialTimeout("tcp", "192.168.1.86:5084", 5*time.Second)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer conn.Close()

	// Skip init notification
	hdr := make([]byte, 10)
	io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// Send GET_ROSPECS (Msg Type 26)
	req := encodeLLRP(26, 501, nil)
	conn.Write(req)

	io.ReadFull(conn, hdr)
	msgType := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	fmt.Printf("GET_ROSPECS_RESPONSE: Type=%d, Len=%d\n", msgType, l)
	fmt.Printf("Payload hex: %x\n", body)
}

func encodeLLRP(msgType uint16, msgID uint32, payload []byte) []byte {
	totalLen := 10 + len(payload)
	buf := make([]byte, totalLen)
	firstWord := (uint16(1) << 10) | (msgType & 0x03FF)
	binary.BigEndian.PutUint16(buf[0:2], firstWord)
	binary.BigEndian.PutUint32(buf[2:6], uint32(totalLen))
	binary.BigEndian.PutUint32(buf[6:10], msgID)
	copy(buf[10:], payload)
	return buf
}
