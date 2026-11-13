package main

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// BitReader provides zero-allocation sequential bit extraction from byte slices
type BitReader struct {
	data   []byte
	bitPos int
}

func NewBitReader(data []byte) *BitReader {
	return &BitReader{data: data, bitPos: 0}
}

func (b *BitReader) ReadBits(n int) (uint64, error) {
	if n <= 0 || n > 64 {
		return 0, fmt.Errorf("invalid bit count: %d", n)
	}
	if b.bitPos+n > len(b.data)*8 {
		return 0, fmt.Errorf("unexpected EOF reading %d bits at bit %d", n, b.bitPos)
	}

	var result uint64
	for i := 0; i < n; i++ {
		byteIndex := (b.bitPos + i) / 8
		bitIndex := 7 - ((b.bitPos + i) % 8)
		bit := (b.data[byteIndex] >> bitIndex) & 1
		result = (result << 1) | uint64(bit)
	}
	b.bitPos += n
	return result, nil
}

// EPCResult contains the normalized semantic fields decoded from any 96-bit EPC tag
type EPCResult struct {
	Standard          string                 `json:"standard"`
	MessageType       string                 `json:"message_type"`
	Header            uint8                  `json:"header"`
	Filter            int                    `json:"filter_value"`
	FilterDesc        string                 `json:"filter_desc"`
	CompanyPrefix     string                 `json:"company_prefix"`
	ItemReference     string                 `json:"item_reference,omitempty"`
	SerialNumber      string                 `json:"serial_number,omitempty"`
	GTIN14            string                 `json:"gtin14,omitempty"`
	ExtensionDigit    string                 `json:"extension_digit,omitempty"`
	SerialReference   string                 `json:"serial_reference,omitempty"`
	SSCC18            string                 `json:"sscc18,omitempty"`
	LocationReference string                 `json:"location_reference,omitempty"`
	GLNExtension      string                 `json:"gln_extension,omitempty"`
	GLN13             string                 `json:"gln13,omitempty"`
	AssetType         string                 `json:"asset_type,omitempty"`
	IndividualAsset   string                 `json:"individual_asset_reference,omitempty"`
	ServiceReference  string                 `json:"service_reference,omitempty"`
	GSRN18            string                 `json:"gsrn18,omitempty"`
	DocumentType      string                 `json:"document_type,omitempty"`
	ComponentPartRef  string                 `json:"component_part_reference,omitempty"`
	CouponReference   string                 `json:"coupon_reference,omitempty"`
	PieceItemRef      string                 `json:"piece_item_reference,omitempty"`
	PieceNumber       string                 `json:"piece_number,omitempty"`
	GeneralManagerNum string                 `json:"general_manager_number,omitempty"`
	ObjectClass       string                 `json:"object_class,omitempty"`
	CanonicalURN      string                 `json:"canonical_urn"`
	RawFields         map[string]interface{} `json:"raw_fields,omitempty"`
}

// GS1 Partition Table Entry (bits and decimal digits)
type partitionEntry struct {
	mBits   int // Company Prefix bits
	lDigits int // Company Prefix digits
	nBits   int // Item/Reference bits
	dDigits int // Item/Reference digits
}

// SGTIN-96 Partition Table
var sgtinPartitionTable = [7]partitionEntry{
	{mBits: 40, lDigits: 12, nBits: 4, dDigits: 1},
	{mBits: 37, lDigits: 11, nBits: 7, dDigits: 2},
	{mBits: 34, lDigits: 10, nBits: 10, dDigits: 3},
	{mBits: 30, lDigits: 9, nBits: 14, dDigits: 4},
	{mBits: 27, lDigits: 8, nBits: 17, dDigits: 5},
	{mBits: 24, lDigits: 7, nBits: 20, dDigits: 6},
	{mBits: 20, lDigits: 6, nBits: 24, dDigits: 7},
}

// SSCC-96 Partition Table
var ssccPartitionTable = [7]partitionEntry{
	{mBits: 40, lDigits: 12, nBits: 18, dDigits: 5},
	{mBits: 37, lDigits: 11, nBits: 21, dDigits: 6},
	{mBits: 34, lDigits: 10, nBits: 24, dDigits: 7},
	{mBits: 30, lDigits: 9, nBits: 28, dDigits: 8},
	{mBits: 27, lDigits: 8, nBits: 31, dDigits: 9},
	{mBits: 24, lDigits: 7, nBits: 34, dDigits: 10},
	{mBits: 20, lDigits: 6, nBits: 38, dDigits: 11},
}

// SGLN-96 Partition Table
var sglnPartitionTable = [7]partitionEntry{
	{mBits: 40, lDigits: 12, nBits: 1, dDigits: 0},
	{mBits: 37, lDigits: 11, nBits: 4, dDigits: 1},
	{mBits: 34, lDigits: 10, nBits: 7, dDigits: 2},
	{mBits: 30, lDigits: 9, nBits: 11, dDigits: 3},
	{mBits: 27, lDigits: 8, nBits: 14, dDigits: 4},
	{mBits: 24, lDigits: 7, nBits: 17, dDigits: 5},
	{mBits: 20, lDigits: 6, nBits: 21, dDigits: 6},
}

// Calculate GS1 Mod-10 Check Digit
func calculateCheckDigit(digits string) string {
	sum := 0
	multiplier := 3
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		sum += d * multiplier
		if multiplier == 3 {
			multiplier = 1
		} else {
			multiplier = 3
		}
	}
	rem := sum % 10
	if rem == 0 {
		return "0"
	}
	return strconv.Itoa(10 - rem)
}

// DecodeEPC decodes a 96-bit hexadecimal string into its respective EPC standard result
func DecodeEPC(hexStr string) (*EPCResult, error) {
	cleanHex := strings.TrimPrefix(strings.TrimSpace(hexStr), "0x")
	cleanHex = strings.TrimPrefix(cleanHex, "0X")
	if len(cleanHex) != 24 {
		return nil, fmt.Errorf("invalid EPC-96 hex length: got %d characters, expected 24", len(cleanHex))
	}

	bytes, err := hex.DecodeString(cleanHex)
	if err != nil {
		return nil, fmt.Errorf("hex decode error: %w", err)
	}

	reader := NewBitReader(bytes)
	headerVal, err := reader.ReadBits(8)
	if err != nil {
		return nil, err
	}
	header := uint8(headerVal)

	switch header {
	case 0x30: // SGTIN-96
		return decodeSGTIN96(reader, header)
	case 0x31: // SSCC-96
		return decodeSSCC96(reader, header)
	case 0x32: // SGLN-96
		return decodeSGLN96(reader, header)
	case 0x33: // GRAI-96
		return decodeGRAI96(reader, header)
	case 0x34: // GIAI-96
		return decodeGIAI96(reader, header)
	case 0x35: // GSRN-96 or GID-96
		return decodeGSRNorGID96(reader, header)
	case 0x3C: // GDTI-96
		return decodeGDTI96(reader, header)
	case 0x3D: // CPI-96
		return decodeCPI96(reader, header)
	case 0x3E: // SGCN-96
		return decodeSGCN96(reader, header)
	case 0x38: // ITIP-96
		return decodeITIP96(reader, header)
	default:
		// Fallback to General Identifier (GID-96) interpretation
		return decodeGenericGID96(bytes, header)
	}
}

func decodeSGTIN96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)

	if partition > 6 {
		return nil, fmt.Errorf("invalid SGTIN partition index: %d", partition)
	}
	p := sgtinPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	itemVal, _ := r.ReadBits(p.nBits)
	serialVal, _ := r.ReadBits(38)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	itemStr := fmt.Sprintf("%0*d", p.dDigits, itemVal)
	serialStr := strconv.FormatUint(serialVal, 10)

	// Indicator digit + company + item digits (13 total digits before check digit)
	indAndItem := itemStr
	var indicatorDigit string
	var itemDigits string
	if len(indAndItem) > 0 {
		indicatorDigit = indAndItem[:1]
		itemDigits = indAndItem[1:]
	} else {
		indicatorDigit = "0"
		itemDigits = "0"
	}

	gtin13 := indicatorDigit + companyStr + itemDigits
	checkDigit := calculateCheckDigit(gtin13)
	gtin14 := gtin13 + checkDigit

	filterDescMap := map[uint64]string{
		0: "All Others",
		1: "Point of Sale (POS) Item",
		2: "Full Case for Transport",
		3: "Reserved",
		4: "Inner Pack Level",
	}

	urn := fmt.Sprintf("urn:epc:tag:sgtin-96:%d.%s.%s.%s", filter, companyStr, itemStr, serialStr)

	return &EPCResult{
		Standard:      "SGTIN-96",
		MessageType:   "EpcSgtinRead",
		Header:        header,
		Filter:        int(filter),
		FilterDesc:    filterDescMap[filter],
		CompanyPrefix: companyStr,
		ItemReference: itemStr,
		SerialNumber:  serialStr,
		GTIN14:        gtin14,
		CanonicalURN:  urn,
	}, nil
}

func decodeSSCC96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)

	if partition > 6 {
		return nil, fmt.Errorf("invalid SSCC partition index: %d", partition)
	}
	p := ssccPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	serialRefVal, _ := r.ReadBits(p.nBits)
	_ , _ = r.ReadBits(24) // Unassigned padding bits in SSCC-96 (24 bits)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	serialRefStr := fmt.Sprintf("%0*d", p.dDigits, serialRefVal)

	extDigit := "0"
	if len(serialRefStr) > 0 {
		extDigit = serialRefStr[:1]
		serialRefStr = serialRefStr[1:]
	}

	sscc17 := extDigit + companyStr + serialRefStr
	checkDigit := calculateCheckDigit(sscc17)
	sscc18 := sscc17 + checkDigit

	urn := fmt.Sprintf("urn:epc:tag:sscc-96:%d.%s.%s%s", filter, companyStr, extDigit, serialRefStr)

	return &EPCResult{
		Standard:        "SSCC-96",
		MessageType:     "EpcSsccRead",
		Header:          header,
		Filter:          int(filter),
		FilterDesc:      "Logistics / Pallet Shipping Container",
		CompanyPrefix:   companyStr,
		ExtensionDigit:  extDigit,
		SerialReference: serialRefStr,
		SSCC18:          sscc18,
		CanonicalURN:    urn,
	}, nil
}

func decodeSGLN96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)

	if partition > 6 {
		return nil, fmt.Errorf("invalid SGLN partition index: %d", partition)
	}
	p := sglnPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	locationVal, _ := r.ReadBits(p.nBits)
	extensionVal, _ := r.ReadBits(41)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	locStr := fmt.Sprintf("%0*d", p.dDigits, locationVal)
	extStr := strconv.FormatUint(extensionVal, 10)

	gln12 := companyStr + locStr
	checkDigit := calculateCheckDigit(gln12)
	gln13 := gln12 + checkDigit

	urn := fmt.Sprintf("urn:epc:tag:sgln-96:%d.%s.%s.%s", filter, companyStr, locStr, extStr)

	return &EPCResult{
		Standard:          "SGLN-96",
		MessageType:       "EpcSglnRead",
		Header:            header,
		Filter:            int(filter),
		CompanyPrefix:     companyStr,
		LocationReference: locStr,
		GLNExtension:      extStr,
		GLN13:             gln13,
		CanonicalURN:      urn,
	}, nil
}

func decodeGRAI96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)
	if partition > 6 {
		partition = 0
	}
	p := sgtinPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	assetVal, _ := r.ReadBits(p.nBits)
	serialVal, _ := r.ReadBits(38)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	assetStr := fmt.Sprintf("%0*d", p.dDigits, assetVal)
	serialStr := strconv.FormatUint(serialVal, 10)
	urn := fmt.Sprintf("urn:epc:tag:grai-96:%d.%s.%s.%s", filter, companyStr, assetStr, serialStr)

	return &EPCResult{
		Standard:      "GRAI-96",
		MessageType:   "EpcGraiRead",
		Header:        header,
		Filter:        int(filter),
		FilterDesc:    "Returnable Asset (Tote/Crate/Keg)",
		CompanyPrefix: companyStr,
		AssetType:     assetStr,
		SerialNumber:  serialStr,
		CanonicalURN:  urn,
	}, nil
}

func decodeGIAI96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)
	if partition > 6 {
		partition = 0
	}
	p := sgtinPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	indAssetVal, _ := r.ReadBits(82 - p.mBits)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	assetRefStr := strconv.FormatUint(indAssetVal, 10)
	urn := fmt.Sprintf("urn:epc:tag:giai-96:%d.%s.%s", filter, companyStr, assetRefStr)

	return &EPCResult{
		Standard:        "GIAI-96",
		MessageType:     "EpcGiaiRead",
		Header:          header,
		Filter:          int(filter),
		FilterDesc:      "Fixed Machinery / Vehicle Asset",
		CompanyPrefix:   companyStr,
		IndividualAsset: assetRefStr,
		CanonicalURN:    urn,
	}, nil
}

func decodeGSRNorGID96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)
	if partition > 6 {
		return decodeGenericGID96(r.data, header)
	}
	p := ssccPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	serviceRefVal, _ := r.ReadBits(p.nBits)
	_, _ = r.ReadBits(24) // padding

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	serviceRefStr := fmt.Sprintf("%0*d", p.dDigits, serviceRefVal)
	gsrn17 := companyStr + serviceRefStr
	checkDigit := calculateCheckDigit(gsrn17)
	gsrn18 := gsrn17 + checkDigit

	urn := fmt.Sprintf("urn:epc:tag:gsrn-96:%d.%s.%s", filter, companyStr, serviceRefStr)

	return &EPCResult{
		Standard:         "GSRN-96",
		MessageType:      "EpcGsrnRead",
		Header:           header,
		Filter:           int(filter),
		FilterDesc:       "Service Recipient / Operator Relation",
		CompanyPrefix:    companyStr,
		ServiceReference: serviceRefStr,
		GSRN18:           gsrn18,
		CanonicalURN:     urn,
	}, nil
}

func decodeGDTI96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)
	if partition > 6 {
		partition = 0
	}
	p := sgtinPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	docTypeVal, _ := r.ReadBits(p.nBits)
	serialVal, _ := r.ReadBits(38)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	docTypeStr := fmt.Sprintf("%0*d", p.dDigits, docTypeVal)
	serialStr := strconv.FormatUint(serialVal, 10)
	urn := fmt.Sprintf("urn:epc:tag:gdti-96:%d.%s.%s.%s", filter, companyStr, docTypeStr, serialStr)

	return &EPCResult{
		Standard:      "GDTI-96",
		MessageType:   "EpcGdtiRead",
		Header:        header,
		Filter:        int(filter),
		FilterDesc:    "Customs / Legal Document",
		CompanyPrefix: companyStr,
		DocumentType:  docTypeStr,
		SerialNumber:  serialStr,
		CanonicalURN:  urn,
	}, nil
}

func decodeCPI96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)
	if partition > 6 {
		partition = 0
	}
	p := sgtinPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	partVal, _ := r.ReadBits(p.nBits)
	serialVal, _ := r.ReadBits(38)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	partStr := fmt.Sprintf("%0*d", p.dDigits, partVal)
	serialStr := strconv.FormatUint(serialVal, 10)
	urn := fmt.Sprintf("urn:epc:tag:cpi-96:%d.%s.%s.%s", filter, companyStr, partStr, serialStr)

	return &EPCResult{
		Standard:         "CPI-96",
		MessageType:      "EpcCpiRead",
		Header:           header,
		Filter:           int(filter),
		FilterDesc:       "Component / Subassembly Part",
		CompanyPrefix:    companyStr,
		ComponentPartRef: partStr,
		SerialNumber:     serialStr,
		CanonicalURN:     urn,
	}, nil
}

func decodeSGCN96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)
	if partition > 6 {
		partition = 0
	}
	p := sgtinPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	couponVal, _ := r.ReadBits(p.nBits)
	serialVal, _ := r.ReadBits(38)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	couponStr := fmt.Sprintf("%0*d", p.dDigits, couponVal)
	serialStr := strconv.FormatUint(serialVal, 10)
	urn := fmt.Sprintf("urn:epc:tag:sgcn-96:%d.%s.%s.%s", filter, companyStr, couponStr, serialStr)

	return &EPCResult{
		Standard:        "SGCN-96",
		MessageType:     "EpcSgcnRead",
		Header:          header,
		Filter:          int(filter),
		FilterDesc:      "High-Value Voucher / Rebate",
		CompanyPrefix:   companyStr,
		CouponReference: couponStr,
		SerialNumber:    serialStr,
		CanonicalURN:    urn,
	}, nil
}

func decodeITIP96(r *BitReader, header uint8) (*EPCResult, error) {
	filter, _ := r.ReadBits(3)
	partition, _ := r.ReadBits(3)
	if partition > 6 {
		partition = 0
	}
	p := sgtinPartitionTable[partition]

	companyVal, _ := r.ReadBits(p.mBits)
	pieceVal, _ := r.ReadBits(p.nBits)
	serialVal, _ := r.ReadBits(38)

	companyStr := fmt.Sprintf("%0*d", p.lDigits, companyVal)
	pieceStr := fmt.Sprintf("%0*d", p.dDigits, pieceVal)
	serialStr := strconv.FormatUint(serialVal, 10)
	urn := fmt.Sprintf("urn:epc:tag:itip-96:%d.%s.%s.%s", filter, companyStr, pieceStr, serialStr)

	return &EPCResult{
		Standard:         "ITIP-96",
		MessageType:      "EpcItipRead",
		Header:           header,
		Filter:           int(filter),
		FilterDesc:       "Variable Piece Good (Coil/Textile/Lumber)",
		CompanyPrefix:    companyStr,
		PieceItemRef:     pieceStr,
		PieceNumber:      serialStr,
		CanonicalURN:     urn,
	}, nil
}

func decodeGenericGID96(bytes []byte, header uint8) (*EPCResult, error) {
	r := NewBitReader(bytes)
	_, _ = r.ReadBits(8) // Header
	generalManager, _ := r.ReadBits(28)
	objectClass, _ := r.ReadBits(24)
	serialVal, _ := r.ReadBits(36)

	genMgrStr := strconv.FormatUint(generalManager, 10)
	objClassStr := strconv.FormatUint(objectClass, 10)
	serialStr := strconv.FormatUint(serialVal, 10)

	urn := fmt.Sprintf("urn:epc:tag:gid-96:%s.%s.%s", genMgrStr, objClassStr, serialStr)

	return &EPCResult{
		Standard:          "GID-96",
		MessageType:       "EpcGidRead",
		Header:            header,
		Filter:            0,
		FilterDesc:        "General Industrial Component",
		GeneralManagerNum: genMgrStr,
		ObjectClass:       objClassStr,
		SerialNumber:      serialStr,
		CanonicalURN:      urn,
	}, nil
}
