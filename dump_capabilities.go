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

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Descartar notificación de inicio
	hdr := make([]byte, 10)
	io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// Petición GET_READER_CAPABILITIES
	req := encodeLLRP(1, 3001, []byte{0x00})
	conn.Write(req)

	io.ReadFull(conn, hdr)
	respLen := binary.BigEndian.Uint32(hdr[2:6])
	respBody := make([]byte, int(respLen)-10)
	io.ReadFull(conn, respBody)

	fmt.Printf("Analizando los %d bytes de la respuesta:\n", len(respBody))
	dumpAllParams(respBody, 0)
}

func dumpAllParams(data []byte, depth int) {
	offset := 0
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		word0 := binary.BigEndian.Uint16(data[offset : offset+2])
		isTV := (word0 & 0x8000) != 0

		if isTV {
			tvType := (word0 >> 8) & 0x7F
			fmt.Printf("%*s[TV Param Type %d] (2 bytes)\n", depth*2, "", tvType)
			offset += 2
			continue
		}

		paramType := word0 & 0x03FF
		paramLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if paramLen < 4 || offset+paramLen > len(data) {
			break
		}

		pData := data[offset+4 : offset+paramLen]
		fmt.Printf("%*s[TLV Param Type %d] (Len=%d)\n", depth*2, "", paramType, paramLen)

		if paramType == 1023 && len(pData) >= 8 {
			pen := binary.BigEndian.Uint32(pData[0:4])
			sub := binary.BigEndian.Uint32(pData[4:8])
			fmt.Printf("%*s  ↳ CustomParam: PEN=%d, Subtype=%d\n", depth*2, "", pen, sub)
		}

		// Si es contenedor, recorrer hijos
		if paramType == 137 || paramType == 143 || paramType == 145 || paramType == 327 || paramType == 328 {
			// Sub-bloques
			childOffset := 0
			if paramType == 137 {
				childOffset = 4 // tras maxAntennas y canReportUTC
			} else if paramType == 143 {
				childOffset = 4 // tras countryCode y standard
			}
			if len(pData) > childOffset {
				dumpAllParams(pData[childOffset:], depth+1)
			}
		}

		offset += paramLen
	}
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
