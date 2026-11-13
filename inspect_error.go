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

	// Send ADD_ROSPEC
	addPayload := buildCompleteROSpec(1)
	addMsg := encodeLLRP(20, 201, addPayload)
	conn.Write(addMsg)

	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	fmt.Printf("ADD_ROSPEC_RESPONSE payload (%d bytes): %x\n", len(body), body)
	dumpDetailedStatus(body)
}

func dumpDetailedStatus(data []byte) {
	offset := 0
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		pType := binary.BigEndian.Uint16(data[offset:offset+2]) & 0x03FF
		pLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if pLen < 4 || offset+pLen > len(data) {
			break
		}

		pBody := data[offset+4 : offset+pLen]
		fmt.Printf("Param Type=%d, Len=%d\n", pType, pLen)

		if pType == 287 { // LLRPStatus
			statusCode := binary.BigEndian.Uint16(pBody[0:2])
			descLen := int(binary.BigEndian.Uint16(pBody[2:4]))
			desc := ""
			if 4+descLen <= len(pBody) {
				desc = string(pBody[4 : 4+descLen])
			}
			fmt.Printf("  -> LLRPStatus Code=%d, Desc='%s'\n", statusCode, desc)
			
			innerOffset := 4 + descLen
			for innerOffset < len(pBody) {
				if innerOffset+4 > len(pBody) {
					break
				}
				inType := binary.BigEndian.Uint16(pBody[innerOffset:innerOffset+2]) & 0x03FF
				inLen := int(binary.BigEndian.Uint16(pBody[innerOffset+2 : innerOffset+4]))
				if inLen < 4 || innerOffset+inLen > len(pBody) {
					break
				}
				inData := pBody[innerOffset+4 : innerOffset+inLen]
				if inType == 288 { // FieldError
					fieldNum := binary.BigEndian.Uint16(inData[0:2])
					errCode := binary.BigEndian.Uint16(inData[2:4])
					fmt.Printf("    ↳ [FieldError 288] FieldNum=%d, ErrorCode=%d\n", fieldNum, errCode)
				} else if inType == 289 { // ParameterError
					paramNum := binary.BigEndian.Uint16(inData[0:2])
					errCode := binary.BigEndian.Uint16(inData[2:4])
					fmt.Printf("    ↳ [ParameterError 289] ParamNum=%d, ErrorCode=%d\n", paramNum, errCode)
				} else {
					fmt.Printf("    ↳ Sub-Param %d, Len=%d\n", inType, inLen)
				}
				innerOffset += inLen
			}
		}
		offset += pLen
	}
}

func buildCompleteROSpec(roSpecID uint32) []byte {
	// --- 1. ROBoundarySpec (Param 178) ---
	var boundary bytes.Buffer
	startTrig := []byte{0, 1} // Type = 1 (Immediate)
	boundary.Write(buildTLV(179, startTrig))
	stopTrig := []byte{0, 0, 0, 0, 0, 0} // Type = 0 (Null), Duration = 0
	boundary.Write(buildTLV(182, stopTrig))
	boundaryBytes := buildTLV(178, boundary.Bytes())

	// --- 2. AISpec (Param 183) ---
	var aiSpec bytes.Buffer
	antList := []byte{0, 1, 0, 1} // 1 antena: Antena 1
	aiSpec.Write(antList)
	aiStopTrig := []byte{0, 0, 0, 0, 0, 0}
	aiSpec.Write(buildTLV(184, aiStopTrig))

	var invParam bytes.Buffer
	invHeader := []byte{0, 1, 1} // SpecID=1, ProtocolID=1
	invParam.Write(invHeader)
	aiSpec.Write(buildTLV(186, invParam.Bytes()))
	aiSpecBytes := buildTLV(183, aiSpec.Bytes())

	// --- 3. ROReportSpec (Param 237) ---
	var roReport bytes.Buffer
	roReport.Write([]byte{1, 0, 1}) // Trigger=1, N=1

	var selector bytes.Buffer
	selector.Write([]byte{0x1F, 0x80})
	epcSelector := []byte{0xC0}
	selector.Write(buildTLV(328, epcSelector))

	roReport.Write(buildTLV(238, selector.Bytes()))
	roReportBytes := buildTLV(237, roReport.Bytes())

	// --- Ensamblado de ROSpec (Param 177) ---
	var roSpecBody bytes.Buffer
	roSpecHdr := make([]byte, 6)
	binary.BigEndian.PutUint32(roSpecHdr[0:4], roSpecID)
	roSpecHdr[4] = 0 // Priority = 0
	roSpecHdr[5] = 1 // CurrentState = Disabled (1)
	roSpecBody.Write(roSpecHdr)
	roSpecBody.Write(boundaryBytes)
	roSpecBody.Write(aiSpecBytes)
	roSpecBody.Write(roReportBytes)

	return buildTLV(177, roSpecBody.Bytes())
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
