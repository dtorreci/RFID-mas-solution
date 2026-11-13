package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	MsgGetReaderCapabilities         uint16 = 1
	MsgGetReaderCapabilitiesResponse uint16 = 11
	MsgReaderEventNotification       uint16 = 63
)

func main() {
	target := "192.168.1.86:5084"
	fmt.Printf("=== Prospección LLRP en Lector Físico %s ===\n", target)

	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Leer mensaje de conexión
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		fmt.Printf("Error leyendo notificación de conexión: %v\n", err)
		return
	}
	firstWord := binary.BigEndian.Uint16(hdr[0:2])
	msgType := firstWord & 0x03FF
	length := binary.BigEndian.Uint32(hdr[2:6])
	payload := make([]byte, int(length)-10)
	_, _ = io.ReadFull(conn, payload)
	fmt.Printf("1. Handshake inicial: Recibido Msg Type %d (%s), Len=%d\n", msgType, getMsgName(msgType), length)

	// Solicitar todas las capacidades
	reqMsg := encodeLLRP(MsgGetReaderCapabilities, 2001, []byte{0x00})
	if _, err := conn.Write(reqMsg); err != nil {
		fmt.Printf("Error enviando petición: %v\n", err)
		return
	}

	if _, err := io.ReadFull(conn, hdr); err != nil {
		fmt.Printf("Error leyendo cabecera de capacidades: %v\n", err)
		return
	}
	msgType = binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
	length = binary.BigEndian.Uint32(hdr[2:6])

	respData := make([]byte, int(length)-10)
	if _, err := io.ReadFull(conn, respData); err != nil {
		fmt.Printf("Error leyendo cuerpo: %v\n", err)
		return
	}
	fmt.Printf("2. Capacidades recibidas: Msg Type %d (%s), Total Bytes=%d\n", msgType, getMsgName(msgType), length)

	parseTLVTree(respData, 0)
}

func parseTLVTree(data []byte, depth int) {
	indent := strings.Repeat("  ", depth)
	offset := 0

	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		typeWord := binary.BigEndian.Uint16(data[offset : offset+2])
		isTV := (typeWord & 0x8000) != 0

		if isTV {
			tvType := uint16((typeWord >> 8) & 0x7F)
			// En general, los TV tienen longitud fija. Los más comunes:
			tvLen := getTVLength(tvType)
			if offset+tvLen > len(data) {
				break
			}
			offset += tvLen
			continue
		}

		paramType := typeWord & 0x03FF
		paramLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if paramLen < 4 || offset+paramLen > len(data) {
			break
		}

		body := data[offset+4 : offset+paramLen]
		pName := getParamName(paramType)

		switch paramType {
		case 137: // GeneralDeviceCapabilities
			maxAnt := binary.BigEndian.Uint16(body[0:2])
			canReportUTC := body[2] != 0
			fmt.Printf("%s• [Param 137: %s] (Len=%d)\n", indent, pName, paramLen)
			fmt.Printf("%s  ↳ Max Antenas: %d | Soporte UTC Timestamp: %v\n", indent, maxAnt, canReportUTC)
			// Parsear sub-parámetros anidados
			if len(body) > 4 {
				parseTLVTree(body[4:], depth+1)
			}

		case 142: // LLRPCapabilities
			fmt.Printf("%s• [Param 142: %s] (Len=%d)\n", indent, pName, paramLen)
			if len(body) >= 8 {
				canDoROSpec := body[0] != 0
				maxPriority := body[1]
				maxROSpecs := binary.BigEndian.Uint32(body[2:6])
				fmt.Printf("%s  ↳ Soporta ROSpec: %v | Max ROSpecs: %d | Max Priority: %d\n", indent, canDoROSpec, maxROSpecs, maxPriority)
			}

		case 143: // RegulatoryCapabilities
			countryCode := binary.BigEndian.Uint16(body[0:2])
			commStandard := binary.BigEndian.Uint16(body[2:4])
			fmt.Printf("%s• [Param 143: %s] CountryCode=%d, CommStandard=%d\n", indent, pName, countryCode, commStandard)
			if len(body) > 4 {
				parseTLVTree(body[4:], depth+1)
			}

		case 145: // UHFBandCapabilities
			fmt.Printf("%s• [Param 145: %s] (Len=%d)\n", indent, pName, paramLen)
			if len(body) > 0 {
				parseTLVTree(body, depth+1)
			}

		case 146: // TransmitPowerLevelTableEntry
			if len(body) >= 4 {
				index := binary.BigEndian.Uint16(body[0:2])
				txPwr := int16(binary.BigEndian.Uint16(body[2:4]))
				if index == 1 || index%10 == 0 {
					fmt.Printf("%s  ↳ Tabla Potencia [Entry %d]: %.2f dBm\n", indent, index, float64(txPwr)/100.0)
				}
			}

		case 1023: // CustomParameter
			if len(body) >= 8 {
				pen := binary.BigEndian.Uint32(body[0:4])
				subtype := binary.BigEndian.Uint32(body[4:8])
				vendorName := "Desconocido"
				if pen == 25882 {
					vendorName = "Impinj"
				} else if pen == 161 {
					vendorName = "Zebra/Motorola"
				} else if pen == 17950 {
					vendorName = "Alien"
				}
				fmt.Printf("%s• [Param 1023: CustomParameter] Fabricante=%s (PEN=%d), Subtipo=%d, Datos=%d bytes\n", indent, vendorName, pen, subtype, len(body)-8)
			}

		default:
			// Otros parámetros TLV estándar
		}

		offset += paramLen
	}
}

func getTVLength(tvType uint16) int {
	switch tvType {
	case 1: // AntennaID
		return 3
	case 2: // FirstSeenTimestampUTC
		return 9
	case 3: // LastSeenTimestampUTC
		return 9
	case 6: // PeakRSSI
		return 2
	case 7: // ChannelIndex
		return 3
	case 8: // TagSeenCount
		return 3
	case 9: // ROSpecID
		return 5
	case 13: // EPC-96
		return 13
	default:
		return 2
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

func getMsgName(t uint16) string {
	switch t {
	case 1:
		return "GET_READER_CAPABILITIES"
	case 11:
		return "GET_READER_CAPABILITIES_RESPONSE"
	case 20:
		return "ADD_ROSPEC"
	case 61:
		return "RO_ACCESS_REPORT"
	case 63:
		return "READER_EVENT_NOTIFICATION"
	default:
		return fmt.Sprintf("Type_%d", t)
	}
}

func getParamName(p uint16) string {
	switch p {
	case 137:
		return "GeneralDeviceCapabilities"
	case 142:
		return "LLRPCapabilities"
	case 143:
		return "RegulatoryCapabilities"
	case 145:
		return "UHFBandCapabilities"
	case 146:
		return "TransmitPowerLevelTableEntry"
	case 1023:
		return "CustomParameter"
	default:
		return fmt.Sprintf("Param_%d", p)
	}
}
