package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// LLRP Messages
const (
	MsgAddROSpec            uint16 = 20
	MsgAddROSpecResponse    uint16 = 30
	MsgDeleteROSpec         uint16 = 21
	MsgDeleteROSpecResponse uint16 = 31
	MsgEnableROSpec         uint16 = 24
	MsgEnableROSpecResponse uint16 = 34
	MsgStartROSpec          uint16 = 22
	MsgStartROSpecResponse  uint16 = 32
	MsgStopROSpec           uint16 = 23
	MsgStopROSpecResponse   uint16 = 33
	MsgROAccessReport       uint16 = 61
)

func main() {
	target := "192.168.1.86:5084"
	fmt.Printf("Conectando a %s para prueba de ROSpec...\n", target)

	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer conn.Close()

	// 1. Leer notificación inicial
	hdr := make([]byte, 10)
	io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Println("1. Handshake inicial completado.")

	// 2. Eliminar ROSpec 1 por si ya existía de una ejecución previa
	delMsg := buildDeleteROSpec(1, 101)
	conn.Write(delMsg)
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Printf("2. DELETE_ROSPEC: Respuesta Type=%d (Status=%s)\n", binary.BigEndian.Uint16(hdr[0:2])&0x3FF, parseLLRPStatus(body))

	// 3. Enviar ADD_ROSPEC con especificación completa
	addPayload := buildCompleteROSpec(1)
	addMsg := encodeLLRP(MsgAddROSpec, 102, addPayload)
	conn.Write(addMsg)

	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Printf("3. ADD_ROSPEC: Respuesta Type=%d (Status=%s)\n", binary.BigEndian.Uint16(hdr[0:2])&0x3FF, parseLLRPStatus(body))

	// 4. Enviar ENABLE_ROSPEC
	enablePayload := make([]byte, 4)
	binary.BigEndian.PutUint32(enablePayload, 1) // ROSpecID = 1
	enableMsg := encodeLLRP(MsgEnableROSpec, 103, enablePayload)
	conn.Write(enableMsg)

	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Printf("4. ENABLE_ROSPEC: Respuesta Type=%d (Status=%s)\n", binary.BigEndian.Uint16(hdr[0:2])&0x3FF, parseLLRPStatus(body))

	// 5. Enviar START_ROSPEC
	startMsg := encodeLLRP(MsgStartROSpec, 104, enablePayload)
	conn.Write(startMsg)

	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Printf("5. START_ROSPEC: Respuesta Type=%d (Status=%s)\n", binary.BigEndian.Uint16(hdr[0:2])&0x3FF, parseLLRPStatus(body))

	// 6. Escuchar durante 5 segundos para ver si llegan reportes RO_ACCESS_REPORT
	fmt.Println("\nEscuchando eventos RO_ACCESS_REPORT durante 5 segundos...")
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			fmt.Printf("Fin de ventana de escucha o timeout: %v\n", err)
			break
		}
		msgType := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		payload := make([]byte, int(msgLen)-10)
		io.ReadFull(conn, payload)

		if msgType == MsgROAccessReport {
			fmt.Printf("-> [RO_ACCESS_REPORT] Recibido paquete de lecturas (%d octetos)!\n", msgLen)
		} else {
			fmt.Printf("-> Mensaje recibido Type=%d, Len=%d\n", msgType, msgLen)
		}
	}

	// 7. Parar ROSpec limpiamente
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	stopMsg := encodeLLRP(MsgStopROSpec, 105, enablePayload)
	conn.Write(stopMsg)
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Println("6. STOP_ROSPEC completado.")
}

// Construye una estructura ROSpec completa y conforme al estándar LLRP 1.0.1
func buildCompleteROSpec(roSpecID uint32) []byte {
	// --- 1. ROBoundarySpec (Param 178) ---
	var boundary bytes.Buffer
	// ROSpecStartTrigger (Param 179): Immediate (Type 0 = Null / 1 = Immediate)
	startTrig := []byte{0, 1} // Type = 1 (Immediate)
	boundary.Write(buildTLV(179, startTrig))
	// ROSpecStopTrigger (Param 182): Null (Type 0 = Null) + Duration=0
	stopTrig := []byte{0, 0, 0, 0, 0, 0} // Type = 0 (Null), Duration = 0
	boundary.Write(buildTLV(182, stopTrig))
	boundaryBytes := buildTLV(178, boundary.Bytes())

	// --- 2. AISpec (Param 183) ---
	var aiSpec bytes.Buffer
	// AntennaIDs (Count = 1, ID = 0 para ALL antennas, o Antena 1)
	// uint16 count + array uint16
	antList := []byte{0, 1, 0, 1} // 1 antena: Antena 1
	aiSpec.Write(antList)

	// AISpecStopTrigger (Param 184): Null (Type 0 = Null) + Duration=0
	aiStopTrig := []byte{0, 0, 0, 0, 0, 0}
	aiSpec.Write(buildTLV(184, aiStopTrig))

	// InventoryParameterSpec (Param 186): ID=1, Protocol=EPCGlobalClass1Gen2 (1)
	var invParam bytes.Buffer
	invHeader := []byte{0, 1, 1} // SpecID=1 (uint16), ProtocolID=1 (uint8: Gen2)
	invParam.Write(invHeader)
	aiSpec.Write(buildTLV(186, invParam.Bytes()))
	aiSpecBytes := buildTLV(183, aiSpec.Bytes())

	// --- 3. ROReportSpec (Param 237) ---
	var roReport bytes.Buffer
	// ROReportTrigger = 1 (Upon_N_Tags_Or_End_Of_AISpec), N = 1
	roReport.Write([]byte{1, 0, 1}) // Trigger=1, N=1 (uint16)

	// TagReportContentSelector (Param 238)
	var selector bytes.Buffer
	// Bitmask de 16 bits para campos estándar:
	// Bit 0: EnableROSpecID (0)
	// Bit 1: EnableSpecIndex (0)
	// Bit 2: EnableInventoryParameterSpecID (0)
	// Bit 3: EnableAntennaID (1)
	// Bit 4: EnableChannelIndex (1)
	// Bit 5: EnableFirstSeenTimestamp (1)
	// Bit 6: EnableLastSeenTimestamp (1)
	// Bit 7: EnablePeakRSSI (1)
	// Bit 8: EnableTagSeenCount (1)
	// Bit 9: EnableAccessSpecID (0)
	// Binario: 0001 1111 1000 0000 = 0x1F80
	selector.Write([]byte{0x1F, 0x80})

	// C1G2EPCMemorySelector (Param 328): EnableCRC=1, EnablePCBits=1
	epcSelector := []byte{0xC0} // bits: 11000000
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

func buildDeleteROSpec(roSpecID uint32, msgID uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, roSpecID)
	return encodeLLRP(MsgDeleteROSpec, msgID, buf)
}

func buildTLV(paramType uint16, value []byte) []byte {
	totalLen := 4 + len(value)
	buf := make([]byte, totalLen)
	binary.BigEndian.PutUint16(buf[0:2], paramType&0x03FF)
	binary.BigEndian.PutUint16(buf[2:4], uint32ToUint16(uint32(totalLen)))
	copy(buf[4:], value)
	return buf
}

func uint32ToUint16(v uint32) uint16 {
	return uint16(v)
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

func parseLLRPStatus(data []byte) string {
	offset := 0
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		pType := binary.BigEndian.Uint16(data[offset:offset+2]) & 0x03FF
		pLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if pType == 287 && pLen >= 8 { // LLRPStatus
			statusCode := binary.BigEndian.Uint16(data[offset+4 : offset+6])
			descLen := int(binary.BigEndian.Uint16(data[offset+6 : offset+8]))
			desc := ""
			if offset+8+descLen <= len(data) {
				desc = string(data[offset+8 : offset+8+descLen])
			}
			return fmt.Sprintf("Code=%d (%s) Desc='%s'", statusCode, getStatusCodeName(statusCode), desc)
		}
		offset += pLen
	}
	return "No status found"
}

func getStatusCodeName(code uint16) string {
	switch code {
	case 0:
		return "M_Success"
	case 100:
		return "M_ParameterError"
	case 101:
		return "M_FieldError"
	case 102:
		return "M_UnexpectedParameter"
	case 103:
		return "M_MissingParameter"
	case 104:
		return "M_DuplicateParameter"
	default:
		return fmt.Sprintf("Code_%d", code)
	}
}
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

	// Send ADD_ROSPEC
	addPayload := buildCompleteROSpec(1)
	addMsg := encodeLLRP(20, 201, addPayload)
	conn.Write(addMsg)

	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	fmt.Printf("ADD_ROSPEC_RESPONSE payload (%d bytes):\n", len(body))
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
			
			// Si hay parámetros anidados dentro de LLRPStatus (FieldError=288, ParameterError=289)
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
