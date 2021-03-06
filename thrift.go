package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"labix.org/v2/mgo/bson"
)

type ThriftMessage struct {
	Ts time.Time

	TcpTuple     TcpTuple
	CmdlineTuple *CmdlineTuple
	Direction    uint8

	start int

	fields []ThriftField

	IsRequest    bool
	HasException bool
	Version      uint32
	Type         uint32
	Method       string
	SeqId        uint32
	Params       string
	ReturnValue  string
	Exceptions   string
	FrameSize    uint32
	Service      string
}

type ThriftField struct {
	Type byte
	Id   uint16

	Value string
}

type ThriftStream struct {
	tcpStream *TcpStream

	data []byte

	parseOffset int
	parseState  int

	// when this is set, don't care about the
	// traffic in this direction. Used to skip large responses.
	skipInput bool

	message *ThriftMessage
}

type ThriftTransaction struct {
	Type         string
	tuple        TcpTuple
	Src          Endpoint
	Dst          Endpoint
	ResponseTime int32
	Ts           int64
	JsTs         time.Time
	ts           time.Time
	cmdline      *CmdlineTuple

	Request *ThriftMessage
	Reply   *ThriftMessage

	timer *time.Timer
}

const (
	ThriftStartState = iota
	ThriftFieldState
)

const (
	ThriftVersionMask = 0xffff0000
	ThriftVersion1    = 0x80010000
	ThriftTypeMask    = 0x000000ff
)

// Thrift types
const (
	ThriftTypeStop   = 0
	ThriftTypeVoid   = 1
	ThriftTypeBool   = 2
	ThriftTypeByte   = 3
	ThriftTypeDouble = 4
	ThriftTypeI16    = 6
	ThriftTypeI32    = 8
	ThriftTypeI64    = 10
	ThriftTypeString = 11
	ThriftTypeStruct = 12
	ThriftTypeMap    = 13
	ThriftTypeSet    = 14
	ThriftTypeList   = 15
	ThriftTypeUtf8   = 16
	ThriftTypeUtf16  = 17
)

// Thrift message types
const (
	_ = iota
	ThriftMsgTypeCall
	ThriftMsgTypeReply
	ThriftMsgTypeException
	ThriftMsgTypeOneway
)

// Thrift protocol types
const (
	ThriftTBinary  = 1
	ThriftTCompact = 2
)

// Thrift transport types
const (
	ThriftTSocket = 1
	ThriftTFramed = 2
)

type Thrift struct {

	// config
	StringMaxSize          int
	CollectionMaxSize      int
	DropAfterNStructFields int
	CaptureReply           bool
	ObfuscateStrings       bool
	Send_request           bool
	Send_response          bool

	TransportType byte
	ProtocolType  byte

	transMap map[HashableTcpTuple]*ThriftTransaction

	PublishQueue chan *ThriftTransaction
	Publisher    *PublisherType
	Idl          *ThriftIdl
}

var ThriftMod Thrift

type tomlThrift struct {
	String_max_size            int
	Collection_max_size        int
	Drop_after_n_struct_fields int
	Transport_type             string
	Protocol_type              string
	Capture_reply              bool
	Obfuscate_strings          bool
	Idl_files                  []string
}

func (thrift *Thrift) InitDefaults() {
	// defaults
	thrift.StringMaxSize = 200
	thrift.CollectionMaxSize = 15
	thrift.DropAfterNStructFields = 500
	thrift.TransportType = ThriftTSocket
	thrift.ProtocolType = ThriftTBinary
	thrift.CaptureReply = true
	thrift.ObfuscateStrings = false
	thrift.Send_request = true
	thrift.Send_response = true
}

func (thrift *Thrift) readConfig() error {
	var err error

	if _ConfigMeta.IsDefined("thrift", "string_max_size") {
		thrift.StringMaxSize = _Config.Thrift.String_max_size
	}
	if _ConfigMeta.IsDefined("thrift", "collection_max_size") {
		thrift.CollectionMaxSize = _Config.Thrift.Collection_max_size
	}
	if _ConfigMeta.IsDefined("thrift", "drop_after_n_struct_fields") {
		thrift.DropAfterNStructFields = _Config.Thrift.Drop_after_n_struct_fields
	}
	if _ConfigMeta.IsDefined("thrift", "transport_type") {
		switch _Config.Thrift.Transport_type {
		case "socket":
			thrift.TransportType = ThriftTSocket
		case "framed":
			thrift.TransportType = ThriftTFramed
		default:
			return fmt.Errorf("Transport type `%s` not known", _Config.Thrift.Transport_type)
		}
	}
	if _ConfigMeta.IsDefined("thrift", "protocol_type") {
		switch _Config.Thrift.Transport_type {
		case "binary":
			thrift.TransportType = ThriftTBinary
		default:
			return fmt.Errorf("Protocol type `%s` not known", _Config.Thrift.Protocol_type)
		}
	}
	if _ConfigMeta.IsDefined("thrift", "capture_reply") {
		thrift.CaptureReply = _Config.Thrift.Capture_reply
	}
	if _ConfigMeta.IsDefined("thrift", "obfuscate_strings") {
		thrift.ObfuscateStrings = _Config.Thrift.Obfuscate_strings
	}
	if _ConfigMeta.IsDefined("thrift", "idl_files") {
		thrift.Idl, err = NewThriftIdl(_Config.Thrift.Idl_files)
		if err != nil {
			return err
		}
	}

	if _ConfigMeta.IsDefined("protocols", "thrift", "send_request") {
		thrift.Send_request = _Config.Protocols["thrift"].Send_request
	}
	if _ConfigMeta.IsDefined("protocols", "thrift", "send_response") {
		thrift.Send_response = _Config.Protocols["thrift"].Send_response
	}

	return nil
}

func (thrift *Thrift) Init(test_mode bool) error {

	thrift.InitDefaults()

	if !test_mode {
		err := thrift.readConfig()
		if err != nil {
			return err
		}
	}

	thrift.transMap = make(map[HashableTcpTuple]*ThriftTransaction, TransactionsHashSize)

	if !test_mode {
		thrift.PublishQueue = make(chan *ThriftTransaction, 1000)
		thrift.Publisher = &Publisher
		go thrift.publishTransactions()
	}

	return nil
}

func (m *ThriftMessage) String() string {
	return fmt.Sprintf("IsRequest: %t Type: %d Method: %s SeqId: %d Params: %s ReturnValue: %s Exceptions: %s",
		m.IsRequest, m.Type, m.Method, m.SeqId, m.Params, m.ReturnValue, m.Exceptions)
}

func (thrift *Thrift) readMessageBegin(s *ThriftStream) (bool, bool) {
	var ok, complete bool
	var offset, off int

	m := s.message

	if len(s.data[s.parseOffset:]) < 9 {
		return true, false // ok, not complete
	}

	sz := Bytes_Ntohl(s.data[s.parseOffset : s.parseOffset+4])
	if int32(sz) < 0 {
		m.Version = sz & ThriftVersionMask
		if m.Version != ThriftVersion1 {
			DEBUG("thrift", "Unexpected version: %d", m.Version)
		}

		DEBUG("thriftdetailed", "version = %d", m.Version)

		offset = s.parseOffset + 4

		DEBUG("thriftdetailed", "offset = %d", offset)

		m.Type = sz & ThriftTypeMask
		m.Method, ok, complete, off = thrift.readString(s.data[offset:])
		if !ok {
			return false, false // not ok, not complete
		}
		if !complete {
			DEBUG("thriftdetailed", "Method name not complete")
			return true, false // ok, not complete
		}
		offset += off

		DEBUG("thriftdetailed", "method = %s", m.Method)
		DEBUG("thriftdetailed", "offset = %d", offset)

		if len(s.data[offset:]) < 4 {
			return true, false // ok, not complete
		}
		m.SeqId = Bytes_Ntohl(s.data[offset : offset+4])
		s.parseOffset = offset + 4
	} else {
		// no version mode
		offset = s.parseOffset

		m.Method, ok, complete, off = thrift.readString(s.data[offset:])
		if !ok {
			return false, false // not ok, not complete
		}
		if !complete {
			DEBUG("thriftdetailed", "Method name not complete")
			return true, false // ok, not complete
		}
		offset += off

		DEBUG("thriftdetailed", "method = %s", m.Method)
		DEBUG("thriftdetailed", "offset = %d", offset)

		if len(s.data[offset:]) < 5 {
			return true, false // ok, not complete
		}

		m.Type = uint32(s.data[offset])
		offset += 1
		m.SeqId = Bytes_Ntohl(s.data[offset : offset+4])
		s.parseOffset = offset + 4
	}

	if m.Type == ThriftMsgTypeCall || m.Type == ThriftMsgTypeOneway {
		m.IsRequest = true
	} else {
		m.IsRequest = false
	}

	return true, true
}

// Functions to decode simple types
// They all have the same signature, returning the string value and the
// number of bytes consumed (off).
type ThriftFieldReader func(data []byte) (value string, ok bool, complete bool, off int)

// thriftReadString caps the returned value to ThriftStringMaxSize but returns the
// off to the end of it.
func (thrift *Thrift) readString(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 4 {
		return "", true, false, 0 // ok, not complete
	}
	sz := int(Bytes_Ntohl(data[:4]))
	if int32(sz) < 0 {
		return "", false, false, 0 // not ok
	}
	if len(data[4:]) < sz {
		return "", true, false, 0 // ok, not complete
	}

	if sz > thrift.StringMaxSize {
		value = string(data[4 : 4+thrift.StringMaxSize])
		value += "..."
	} else {
		value = string(data[4 : 4+sz])
	}
	off = 4 + sz

	return value, true, true, off // all good
}

func (thrift *Thrift) readAndQuoteString(data []byte) (value string, ok bool, complete bool, off int) {
	value, ok, complete, off = thrift.readString(data)
	if value == "" {
		value = `""`
	} else if thrift.ObfuscateStrings {
		value = `"*"`
	} else {
		if utf8.ValidString(value) {
			value = strconv.Quote(value)
		} else {
			value = hex.EncodeToString([]byte(value))
		}
	}

	return value, ok, complete, off
}

func (thrift *Thrift) readBool(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 1 {
		return "", true, false, 0
	}
	if data[0] == byte(0) {
		value = "false"
	} else {
		value = "true"
	}

	return value, true, true, 1
}

func (thrift *Thrift) readByte(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 1 {
		return "", true, false, 0
	}
	value = strconv.Itoa(int(data[0]))

	return value, true, true, 1
}

func (thrift *Thrift) readDouble(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 8 {
		return "", true, false, 0
	}

	bits := binary.BigEndian.Uint64(data[:8])
	double := math.Float64frombits(bits)
	value = strconv.FormatFloat(double, 'f', -1, 64)

	return value, true, true, 8
}

func (thrift *Thrift) readI16(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 2 {
		return "", true, false, 0
	}
	i16 := Bytes_Ntohs(data[:2])
	value = strconv.Itoa(int(i16))

	return value, true, true, 2
}

func (thrift *Thrift) readI32(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 4 {
		return "", true, false, 0
	}
	i32 := Bytes_Ntohl(data[:4])
	value = strconv.Itoa(int(i32))

	return value, true, true, 4
}

func (thrift *Thrift) readI64(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 8 {
		return "", true, false, 0
	}
	i64 := Bytes_Ntohll(data[:8])
	value = strconv.FormatInt(int64(i64), 10)

	return value, true, true, 8
}

// Common implementation for lists and sets (they share the same binary repr).
func (thrift *Thrift) readListOrSet(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 5 {
		return "", true, false, 0
	}
	type_ := data[0]

	funcReader, typeFound := thrift.funcReadersByType(type_)
	if !typeFound {
		DEBUG("thrift", "Field type %d not known", type_)
		return "", false, false, 0
	}

	sz := int(Bytes_Ntohl(data[1:5]))
	if sz < 0 {
		DEBUG("thrift", "List/Set too big: %d", sz)
		return "", false, false, 0
	}

	fields := []string{}
	offset := 5

	for i := 0; i < sz; i++ {
		value, ok, complete, bytesRead := funcReader(data[offset:])
		if !ok {
			return "", false, false, 0
		}
		if !complete {
			return "", true, false, 0
		}

		if i < thrift.CollectionMaxSize {
			fields = append(fields, value)
		} else if i == thrift.CollectionMaxSize {
			fields = append(fields, "...")
		}
		offset += bytesRead
	}

	return strings.Join(fields, ", "), true, true, offset
}

func (thrift *Thrift) readSet(data []byte) (value string, ok bool, complete bool, off int) {
	value, ok, complete, off = thrift.readListOrSet(data)
	if value != "" {
		value = "{" + value + "}"
	}
	return value, ok, complete, off
}

func (thrift *Thrift) readList(data []byte) (value string, ok bool, complete bool, off int) {
	value, ok, complete, off = thrift.readListOrSet(data)
	if value != "" {
		value = "[" + value + "]"
	}
	return value, ok, complete, off
}

func (thrift *Thrift) readMap(data []byte) (value string, ok bool, complete bool, off int) {
	if len(data) < 6 {
		return "", true, false, 0
	}
	type_key := data[0]
	type_value := data[1]

	funcReaderKey, typeFound := thrift.funcReadersByType(type_key)
	if !typeFound {
		DEBUG("thrift", "Field type %d not known", type_key)
		return "", false, false, 0
	}

	funcReaderValue, typeFound := thrift.funcReadersByType(type_value)
	if !typeFound {
		DEBUG("thrift", "Field type %d not known", type_value)
		return "", false, false, 0
	}

	sz := int(Bytes_Ntohl(data[2:6]))
	if sz < 0 {
		DEBUG("thrift", "Map too big: %d", sz)
		return "", false, false, 0
	}

	fields := []string{}
	offset := 6

	for i := 0; i < sz; i++ {
		key, ok, complete, bytesRead := funcReaderKey(data[offset:])
		if !ok {
			return "", false, false, 0
		}
		if !complete {
			return "", true, false, 0
		}
		offset += bytesRead

		value, ok, complete, bytesRead := funcReaderValue(data[offset:])
		if !ok {
			return "", false, false, 0
		}
		if !complete {
			return "", true, false, 0
		}
		offset += bytesRead

		if i < thrift.CollectionMaxSize {
			fields = append(fields, key+": "+value)
		} else if i == thrift.CollectionMaxSize {
			fields = append(fields, "...")
		}
	}

	return "{" + strings.Join(fields, ", ") + "}", true, true, offset
}

func (thrift *Thrift) readStruct(data []byte) (value string, ok bool, complete bool, off int) {

	var bytesRead int
	offset := 0
	fields := []ThriftField{}

	// Loop until hitting a STOP or reaching the maximum number of elements
	// we follow in a stream (at which point, we assume we interpreted something
	// wrong).
	for i := 0; ; i++ {
		var field ThriftField

		if i >= thrift.DropAfterNStructFields {
			DEBUG("thrift", "Too many fields in struct. Dropping as error")
			return "", false, false, 0
		}

		if len(data) < 1 {
			return "", true, false, 0
		}

		field.Type = byte(data[offset])
		offset += 1
		if field.Type == ThriftTypeStop {
			return thrift.formatStruct(fields, false, []*string{}), true, true, offset
		}

		if len(data[offset:]) < 2 {
			return "", true, false, 0 // not complete
		}

		field.Id = Bytes_Ntohs(data[offset : offset+2])
		offset += 2

		funcReader, typeFound := thrift.funcReadersByType(field.Type)
		if !typeFound {
			DEBUG("thrift", "Field type %d not known", field.Type)
			return "", false, false, 0
		}

		field.Value, ok, complete, bytesRead = funcReader(data[offset:])

		if !ok {
			return "", false, false, 0
		}
		if !complete {
			return "", true, false, 0
		}
		fields = append(fields, field)
		offset += bytesRead
	}
}

func (thrift *Thrift) formatStruct(fields []ThriftField, resolve_names bool,
	fieldnames []*string) string {

	toJoin := []string{}
	for i, field := range fields {
		if i == thrift.CollectionMaxSize {
			toJoin = append(toJoin, "...")
			break
		}
		if resolve_names && int(field.Id) < len(fieldnames) && fieldnames[field.Id] != nil {
			toJoin = append(toJoin, *fieldnames[field.Id]+": "+field.Value)
		} else {
			toJoin = append(toJoin, strconv.Itoa(int(field.Id))+": "+field.Value)
		}
	}
	return "(" + strings.Join(toJoin, ", ") + ")"
}

// Dictionary wrapped in a function to avoid "initialization loop"
func (thrift *Thrift) funcReadersByType(type_ byte) (func_ ThriftFieldReader, exists bool) {
	switch type_ {
	case ThriftTypeBool:
		return thrift.readBool, true
	case ThriftTypeByte:
		return thrift.readByte, true
	case ThriftTypeDouble:
		return thrift.readDouble, true
	case ThriftTypeI16:
		return thrift.readI16, true
	case ThriftTypeI32:
		return thrift.readI32, true
	case ThriftTypeI64:
		return thrift.readI64, true
	case ThriftTypeString:
		return thrift.readAndQuoteString, true
	case ThriftTypeList:
		return thrift.readList, true
	case ThriftTypeSet:
		return thrift.readSet, true
	case ThriftTypeMap:
		return thrift.readMap, true
	case ThriftTypeStruct:
		return thrift.readStruct, true
	default:
		return nil, false
	}
}

func (thrift *Thrift) readField(s *ThriftStream) (ok bool, complete bool, field *ThriftField) {

	var off int

	field = new(ThriftField)

	if len(s.data) == 0 {
		return true, false, nil // ok, not complete
	}
	field.Type = byte(s.data[s.parseOffset])
	offset := s.parseOffset + 1
	if field.Type == ThriftTypeStop {
		s.parseOffset = offset
		return true, true, nil // done
	}

	if len(s.data[offset:]) < 2 {
		return true, false, nil // ok, not complete
	}
	field.Id = Bytes_Ntohs(s.data[offset : offset+2])
	offset += 2

	funcReader, typeFound := thrift.funcReadersByType(field.Type)
	if !typeFound {
		DEBUG("thrift", "Field type %d not known", field.Type)
		return false, false, nil
	}

	field.Value, ok, complete, off = funcReader(s.data[offset:])

	if !ok {
		return false, false, nil
	}
	if !complete {
		return true, false, nil
	}
	offset += off

	s.parseOffset = offset
	return true, false, field
}

func (thrift *Thrift) messageParser(s *ThriftStream) (bool, bool) {
	var ok, complete bool
	var m = s.message

	for s.parseOffset < len(s.data) {
		switch s.parseState {
		case ThriftStartState:
			m.start = s.parseOffset
			if thrift.TransportType == ThriftTFramed {
				// read I32
				if len(s.data) < 4 {
					return true, false
				}
				m.FrameSize = Bytes_Ntohl(s.data[:4])
				s.parseOffset = 4
			}

			ok, complete = thrift.readMessageBegin(s)
			if !ok {
				return false, false
			}
			if !complete {
				return true, false
			}

			if !m.IsRequest && !thrift.CaptureReply {
				// don't actually read the result
				DEBUG("thrift", "Don't capture reply")
				m.ReturnValue = ""
				m.Exceptions = ""
				return true, true
			}
			s.parseState = ThriftFieldState
		case ThriftFieldState:
			ok, complete, field := thrift.readField(s)
			if !ok {
				return false, false
			}
			if complete {
				// done
				var method *ThriftIdlMethod = nil
				if thrift.Idl != nil {
					method = thrift.Idl.FindMethod(m.Method)
				}
				if m.IsRequest {
					if method != nil {
						m.Params = thrift.formatStruct(m.fields, true, method.Params)

						m.Service = method.Service.Name
					} else {
						m.Params = thrift.formatStruct(m.fields, false, nil)
					}
				} else {
					if len(m.fields) > 1 {
						WARN("Thrift RPC response with more than field. Ignoring all but first")
					}
					if len(m.fields) > 0 {
						field := m.fields[0]
						if field.Id == 0 {
							m.ReturnValue = field.Value
							m.Exceptions = ""
						} else {
							m.ReturnValue = ""
							if method != nil {
								m.Exceptions = thrift.formatStruct(m.fields, true, method.Exceptions)
							} else {
								m.Exceptions = thrift.formatStruct(m.fields, false, nil)
							}
							m.HasException = true
						}
					}
				}
				return true, true
			}
			if field == nil {
				return true, false // ok, not complete
			}

			m.fields = append(m.fields, *field)
		}
	}

	return true, false
}

func (stream *ThriftStream) PrepareForNewMessage(flush bool) {
	if flush {
		stream.data = []byte{}
	} else {
		stream.data = stream.data[stream.parseOffset:]
	}
	DEBUG("thrift", "remaining data: [%s]", stream.data)
	stream.parseOffset = 0
	stream.message = nil
	stream.parseState = ThriftStartState
}

func (thrift *Thrift) Parse(pkt *Packet, tcp *TcpStream, dir uint8) {

	defer RECOVER("ParseThrift exception")

	stream := tcp.thriftData[dir]

	if stream == nil {
		stream = &ThriftStream{
			tcpStream: tcp,
			data:      pkt.payload,
			message:   &ThriftMessage{Ts: pkt.ts},
		}
		tcp.thriftData[dir] = stream
	} else {
		if stream.skipInput {
			// stream currently suspended in this direction
			return
		}
		// concatenate bytes
		stream.data = append(stream.data, pkt.payload...)
		if len(stream.data) > TCP_MAX_DATA_IN_STREAM {
			DEBUG("thrift", "Stream data too large, dropping TCP stream")
			tcp.thriftData[dir] = nil
			return
		}
	}

	for len(stream.data) > 0 {
		if stream.message == nil {
			stream.message = &ThriftMessage{Ts: pkt.ts}
		}

		ok, complete := thrift.messageParser(tcp.thriftData[dir])

		if !ok {
			// drop this tcp stream. Will retry parsing with the next
			// segment in it
			tcp.thriftData[dir] = nil
			DEBUG("thrift", "Ignore Thrift message. Drop tcp stream. Try parsing with the next segment")
			return
		}

		if complete {
			var flush bool = false

			if stream.message.IsRequest {
				DEBUG("thrift", "Thrift request message: %s", stream.message.Method)
				if !thrift.CaptureReply {
					// enable the stream in the other direction to get the reply
					stream_rev := tcp.thriftData[1-dir]
					if stream_rev != nil {
						stream_rev.skipInput = false
					}
				}
			} else {
				DEBUG("thrift", "Thrift response message: %s", stream.message.Method)
				if !thrift.CaptureReply {
					// disable stream in this direction
					stream.skipInput = true

					// and flush current data
					flush = true
				}
			}

			// all ok, go to next level
			stream.message.TcpTuple = TcpTupleFromIpPort(tcp.tuple, tcp.id)
			stream.message.Direction = dir
			stream.message.CmdlineTuple = procWatcher.FindProcessesTuple(tcp.tuple)
			if stream.message.FrameSize == 0 {
				stream.message.FrameSize = uint32(stream.parseOffset - stream.message.start)
			}
			thrift.handleThrift(stream.message)

			// and reset message
			stream.PrepareForNewMessage(flush)
		} else {
			// wait for more data
			break
		}
	}

}

func (thrift *Thrift) handleThrift(msg *ThriftMessage) {
	if msg.IsRequest {
		thrift.receivedRequest(msg)
	} else {
		thrift.receivedReply(msg)
	}
}

func (thrift *Thrift) receivedRequest(msg *ThriftMessage) {
	tuple := msg.TcpTuple

	trans := thrift.transMap[tuple.raw]
	if trans != nil {
		DEBUG("thrift", "Two requests without reply, assuming the old one is oneway")
		thrift.PublishQueue <- trans
	}

	trans = &ThriftTransaction{
		Type:  "thrift",
		tuple: tuple,
	}
	thrift.transMap[tuple.raw] = trans

	trans.ts = msg.Ts
	trans.Ts = int64(trans.ts.UnixNano() / 1000)
	trans.JsTs = msg.Ts
	trans.Src = Endpoint{
		Ip:   msg.TcpTuple.Src_ip.String(),
		Port: msg.TcpTuple.Src_port,
		Proc: string(msg.CmdlineTuple.Src),
	}
	trans.Dst = Endpoint{
		Ip:   msg.TcpTuple.Dst_ip.String(),
		Port: msg.TcpTuple.Dst_port,
		Proc: string(msg.CmdlineTuple.Dst),
	}
	if msg.Direction == TcpDirectionReverse {
		trans.Src, trans.Dst = trans.Dst, trans.Src
	}

	trans.Request = msg

	if trans.timer != nil {
		trans.timer.Stop()
	}
	trans.timer = time.AfterFunc(TransactionTimeout, func() { thrift.expireTransaction(trans) })

}

func (thrift *Thrift) receivedReply(msg *ThriftMessage) {

	// we need to search the request first.
	tuple := msg.TcpTuple

	trans := thrift.transMap[tuple.raw]
	if trans == nil {
		DEBUG("thrift", "Response from unknown transaction. Ignoring: %v", tuple)
		return
	}

	if trans.Request.Method != msg.Method {
		DEBUG("thrift", "Response from another request received '%s' '%s'"+
			". Ignoring.", trans.Request.Method, msg.Method)
		return
	}

	trans.Reply = msg

	trans.ResponseTime = int32(msg.Ts.Sub(trans.ts).Nanoseconds() / 1e6) // resp_time in milliseconds

	thrift.PublishQueue <- trans

	DEBUG("thrift", "Transaction queued")

	// remove from map
	thrift.transMap[tuple.raw] = nil
	if trans.timer != nil {
		trans.timer.Stop()
	}
}

func (thrift *Thrift) ReceivedFin(tcp *TcpStream, dir uint8) {
	tuple := TcpTupleFromIpPort(tcp.tuple, tcp.id)

	trans := thrift.transMap[tuple.raw]
	if trans != nil {
		if trans.Request != nil && trans.Reply == nil {
			DEBUG("thrift", "FIN and had only one transaction. Assuming one way")
			thrift.PublishQueue <- trans
			delete(thrift.transMap, trans.tuple.raw)
			if trans.timer != nil {
				trans.timer.Stop()
			}
		}
	}
}

func (thrift *Thrift) publishTransactions() {
	for t := range thrift.PublishQueue {
		event := Event{}

		event.Type = "thrift"
		if t.Reply != nil && t.Reply.HasException {
			event.Status = ERROR_STATUS
		} else {
			event.Status = OK_STATUS
		}
		event.ResponseTime = t.ResponseTime
		event.Thrift = bson.M{}

		if t.Request != nil {
			event.Thrift = bson.M{
				"request": bson.M{
					"method": t.Request.Method,
					"params": t.Request.Params,
					"size":   t.Request.FrameSize,
				},
				"service": t.Request.Service,
			}

			if thrift.Send_request {
				event.RequestRaw = fmt.Sprintf("%s%s", t.Request.Method,
					t.Request.Params)
			}
		}

		if t.Reply != nil {
			event.Thrift = bson_concat(event.Thrift, bson.M{
				"reply": bson.M{
					"returnValue": t.Reply.ReturnValue,
					"exceptions":  t.Reply.Exceptions,
					"size":        t.Reply.FrameSize,
				},
			})

			if thrift.Send_response {
				if !t.Reply.HasException {
					event.ResponseRaw = t.Reply.ReturnValue
				} else {
					event.ResponseRaw = fmt.Sprintf("Exceptions: %s",
						t.Reply.Exceptions)
				}
			}
		}

		if thrift.Publisher != nil {
			thrift.Publisher.PublishEvent(t.ts, &t.Src, &t.Dst, &event)
		}

		DEBUG("thrift", "Published event")
	}
}

func (thrift *Thrift) expireTransaction(trans *ThriftTransaction) {
	// TODO - also publish?
	// remove from map
	delete(thrift.transMap, trans.tuple.raw)
}
