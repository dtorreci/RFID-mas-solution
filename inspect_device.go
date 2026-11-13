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

	// Skip connection notification
	hdr := make([]byte, 10)
	io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// Send GET_READER_CAPABILITIES
	req := encodeLLRP(1, 4001, []byte{0x00})
	conn.Write(req)

	io.ReadFull(conn, hdr)
	respLen := binary.BigEndian.Uint32(hdr[2:6])
	respBody := make([]byte, int(respLen)-10)
	io.ReadFull(conn, respBody)

	// Parse Param 137 (GeneralDeviceCapabilities)
	offset := 0
	for offset < len(respBody) {
		if offset+4 > len(respBody) {
			break
		}
		pType := binary.BigEndian.Uint16(respBody[offset:offset+2]) & 0x03FF
		pLen := int(binary.BigEndian.Uint16(respBody[offset+2 : offset+4]))

		if pType == 137 {
			pData := respBody[offset+4 : offset+pLen]
			maxAnt := binary.BigEndian.Uint16(pData[0:2])
			canUTC := pData[2] != 0
			devMfg := binary.BigEndian.Uint32(pData[3:7])
			model := binary.BigEndian.Uint32(pData[7:11])
			
			// String de firmware
			fwLen := int(binary.BigEndian.Uint16(pData[11:13]))
			var fwStr string
			if 13+fwLen <= len(pData) {
				fwStr = string(pData[13 : 13+fwLen])
			}

			fmt.Println("=== Datos Generales del Lector R2000 (192.168.1.86) ===")
			fmt.Printf("• Número de Antenas: %d\n", maxAnt)
			fmt.Printf("• Soporta UTC Timestamp: %v\n", canUTC)
			fmt.Printf("• Fabricante ID: %d (0x%08X)\n", devMfg, devMfg)
			fmt.Printf("• Modelo ID: %d (0x%08X)\n", model, model)
			fmt.Printf("• Versión Firmware: %s\n", fwStr)
			break
		}
		offset += pLen
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
