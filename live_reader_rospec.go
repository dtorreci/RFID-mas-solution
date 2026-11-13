package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

func main() {
	target := "192.168.1.86:5084"
	fmt.Printf("Conectando a %s...\n", target)

	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer conn.Close()

	// 1. Skip reader init notification
	hdr := make([]byte, 10)
	io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Println("1. Handshake inicial OK.")

	// 2. Delete any old ROSpec 1
	delMsg := encodeLLRP(21, 101, uint32Bytes(1))
	conn.Write(delMsg)
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Println("2. DELETE_ROSPEC completado.")

	// 3. Send ADD_ROSPEC (Type 20)
	addPayload := buildStrictROSpec(1)
	addMsg := encodeLLRP(20, 102, addPayload)
	conn.Write(addMsg)

	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	status, code := parseStatus(body)
	fmt.Printf("3. ADD_ROSPEC Response: Status=%s (Code=%d)\n", status, code)

	if code != 0 {
		fmt.Printf("Payload hex (%d bytes): %x\n", len(body), body)
		return
	}

	// 4. Send ENABLE_ROSPEC (Type 24)
	enableMsg := encodeLLRP(24, 103, uint32Bytes(1))
	conn.Write(enableMsg)
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	status, code = parseStatus(body)
	fmt.Printf("4. ENABLE_ROSPEC Response: Status=%s (Code=%d)\n", status, code)

	// 5. Send START_ROSPEC (Type 22)
	startMsg := encodeLLRP(22, 104, uint32Bytes(1))
	conn.Write(startMsg)
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	status, code = parseStatus(body)
	fmt.Printf("5. START_ROSPEC Response: Status=%s (Code=%d)\n", status, code)

	// 6. Listen for tags
	fmt.Println("\n>>> ¡ROSpec ACTIVA! Escuchando tags en vivo por las antenas durante 10 segundos... <<<")
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	tagCount := 0
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			fmt.Printf("Fin de ventana de escucha (%v)\n", err)
			break
		}
		msgType := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		payload := make([]byte, int(msgLen)-10)
		io.ReadFull(conn, payload)

		if msgType == 61 { // RO_ACCESS_REPORT
			tagCount++
			fmt.Printf("-> [RO_ACCESS_REPORT #%d] Recibido! Tamaño=%d octetos\n", tagCount, msgLen)
		} else if msgType == 62 { // KEEPALIVE
			ack := encodeLLRP(72, binary.BigEndian.Uint32(hdr[6:10]), nil)
			conn.Write(ack)
			fmt.Println("-> KEEPALIVE respondido con ACK.")
		} else {
			fmt.Printf("-> Mensaje recibido Type=%d, Len=%d\n", msgType, msgLen)
		}
	}

	// 7. Stop ROSpec
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	stopMsg := encodeLLRP(23, 105, uint32Bytes(1))
	conn.Write(stopMsg)
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Println("7. STOP_ROSPEC finalizado con éxito.")
}

func buildStrictROSpec(roSpecID uint32) []byte {
	// --- 1. ROBoundarySpec (Param 178) ---
	var boundary bytes.Buffer
	// ROSpecStartTrigger (Param 179): Type=1 (Immediate) -> 1 byte body
	boundary.Write(buildTLV(179, []byte{1}))
	// ROSpecStopTrigger (Param 182): Type=0 (Null), Duration=0 -> 5 bytes body
	stopTrig := make([]byte, 5)
	stopTrig[0] = 0 // Type = Null
	binary.BigEndian.PutUint32(stopTrig[1:5], 0)
	boundary.Write(buildTLV(182, stopTrig))
	boundaryTLV := buildTLV(178, boundary.Bytes())

	// --- 2. AISpec (Param 183) ---
	var aiSpec bytes.Buffer
	// AntennaIDs: uint16 count = 1, uint16 antennaID = 0 (All Antennas)
	antList := []byte{0, 1, 0, 0}
	aiSpec.Write(antList)

	// AISpecStopTrigger (Param 184): Type=0 (Null), Duration=0 -> 5 bytes body
	aiStopTrig := make([]byte, 5)
	aiStopTrig[0] = 0 // Null
	binary.BigEndian.PutUint32(aiStopTrig[1:5], 0)
	aiSpec.Write(buildTLV(184, aiStopTrig))

	// InventoryParameterSpec (Param 186): ID=1, Protocol=1 (Gen2) -> 3 bytes body
	invParam := []byte{0, 1, 1}
	aiSpec.Write(buildTLV(186, invParam))
	aiSpecTLV := buildTLV(183, aiSpec.Bytes())

	// --- 3. ROReportSpec (Param 237) ---
	var roReport bytes.Buffer
	// ROReportTrigger = 1 (Upon_N_Tags_Or_End_Of_AISpec), N = 1 -> 3 bytes body
	roReport.Write([]byte{1, 0, 1})

	// TagReportContentSelector (Param 238)
	var selector bytes.Buffer
	// Bitmask 16 bits:
	// EnableROSpecID(0), EnableSpecIndex(0), EnableInventoryParameterSpecID(0)
	// EnableAntennaID(1), EnableChannelIndex(1), EnableFirstSeenTimestamp(1)
	// EnableLastSeenTimestamp(1), EnablePeakRSSI(1), EnableTagSeenCount(1), EnableAccessSpecID(0)
	// 0001 1111 1000 0000 = 0x1F80
	selector.Write([]byte{0x1F, 0x80})

	roReport.Write(buildTLV(238, selector.Bytes()))
	roReportTLV := buildTLV(237, roReport.Bytes())

	// --- Ensamblado ROSpec (Param 177) ---
	var roSpec bytes.Buffer
	hdr := make([]byte, 6)
	binary.BigEndian.PutUint32(hdr[0:4], roSpecID)
	hdr[4] = 0 // Priority = 0
	hdr[5] = 0 // CurrentState = Disabled (0)
	roSpec.Write(hdr)
	roSpec.Write(boundaryTLV)
	roSpec.Write(aiSpecTLV)
	roSpec.Write(roReportTLV)

	return buildTLV(177, roSpec.Bytes())
}

func uint32Bytes(v uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, v)
	return buf
}

func buildTLV(paramType uint16, value []byte) []byte {
	totalLen := 4 + len(value)
	buf := make([]byte, totalLen)
	binary.BigEndian.PutUint16(buf[0:2], paramType&0x03FF)
	binary.BigEndian.PutUint16(buf[2:4], uint16(totalLen))
	copy(buf[4:], value)
	return buf
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

func parseStatus(data []byte) (string, uint16) {
	for offset := 0; offset+4 <= len(data); {
		pType := binary.BigEndian.Uint16(data[offset:offset+2]) & 0x03FF
		pLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if pType == 287 && pLen >= 8 {
			code := binary.BigEndian.Uint16(data[offset+4 : offset+6])
			descLen := int(binary.BigEndian.Uint16(data[offset+6 : offset+8]))
			desc := "Success"
			if descLen > 0 && offset+8+descLen <= len(data) {
				desc = string(data[offset+8 : offset+8+descLen])
			}
			return desc, code
		}
		offset += pLen
	}
	return "Unknown", 999
}
