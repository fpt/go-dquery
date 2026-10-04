package value

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Key encoding
//
// Each scalar is encoded as a tag byte followed by a payload. Encodings are
// self-delimiting and order-preserving, so the concatenation of element
// encodings preserves tuple order and a tuple prefix encodes to a byte prefix:
//
//	Compare(a, b) == bytes.Compare(EncodeKey(nil, a), EncodeKey(nil, b))
//
// Tags follow Kind order so that cross-kind order matches Compare.
const (
	tagNull      = 0x05
	tagFalse     = 0x10
	tagTrue      = 0x11
	tagInt       = 0x20
	tagFloat     = 0x30
	tagString    = 0x40
	tagBytes     = 0x50
	tagTimestamp = 0x60
)

// Escaping for variable-length payloads: 0x00 -> 0x00 0xFF, terminated by
// 0x00 0x01. The terminator sorts below any escaped 0x00 and below any other
// byte, which makes "a" < "a\x00" < "ab".
const (
	escByte  = 0x00
	escEsc   = 0xFF
	escTerm  = 0x01
	signFlip = uint64(1) << 63
)

var ErrNotEncodable = errors.New("value: not encodable as key")

// EncodeKey appends the key encoding of v to dst.
func EncodeKey(dst []byte, v Value) ([]byte, error) {
	switch v.kind {
	case KindNull:
		return append(dst, tagNull), nil
	case KindBool:
		if v.Bool() {
			return append(dst, tagTrue), nil
		}
		return append(dst, tagFalse), nil
	case KindInt:
		return binary.BigEndian.AppendUint64(append(dst, tagInt), v.num^signFlip), nil
	case KindFloat:
		return binary.BigEndian.AppendUint64(append(dst, tagFloat), floatKey(v.num)), nil
	case KindString:
		return appendEscaped(append(dst, tagString), v.str), nil
	case KindBytes:
		return appendEscaped(append(dst, tagBytes), v.str), nil
	case KindTimestamp:
		return binary.BigEndian.AppendUint64(append(dst, tagTimestamp), v.num^signFlip), nil
	}
	return dst, fmt.Errorf("%w: %s", ErrNotEncodable, v.kind)
}

// EncodeTuple appends the encodings of all elements of t to dst.
func EncodeTuple(dst []byte, t Tuple) ([]byte, error) {
	var err error
	for _, v := range t {
		if dst, err = EncodeKey(dst, v); err != nil {
			return dst, err
		}
	}
	return dst, nil
}

// DecodeKey decodes one value from the front of b and returns the remainder.
func DecodeKey(b []byte) (Value, []byte, error) {
	if len(b) == 0 {
		return Value{}, nil, errors.New("value: decode: empty input")
	}
	tag, b := b[0], b[1:]
	switch tag {
	case tagNull:
		return Null, b, nil
	case tagFalse:
		return Bool(false), b, nil
	case tagTrue:
		return Bool(true), b, nil
	case tagInt, tagFloat, tagTimestamp:
		if len(b) < 8 {
			return Value{}, nil, errors.New("value: decode: short fixed-width value")
		}
		u := binary.BigEndian.Uint64(b)
		switch tag {
		case tagInt:
			return Value{kind: KindInt, num: u ^ signFlip}, b[8:], nil
		case tagFloat:
			return Value{kind: KindFloat, num: floatUnkey(u)}, b[8:], nil
		default:
			return Value{kind: KindTimestamp, num: u ^ signFlip}, b[8:], nil
		}
	case tagString, tagBytes:
		s, rest, err := readEscaped(b)
		if err != nil {
			return Value{}, nil, err
		}
		if tag == tagString {
			return Value{kind: KindString, str: s}, rest, nil
		}
		return Value{kind: KindBytes, str: s}, rest, nil
	}
	return Value{}, nil, fmt.Errorf("value: decode: unknown tag 0x%02x", tag)
}

// DecodeTuple decodes exactly n values from the front of b.
func DecodeTuple(b []byte, n int) (Tuple, []byte, error) {
	t := make(Tuple, n)
	var err error
	for i := range n {
		if t[i], b, err = DecodeKey(b); err != nil {
			return nil, nil, err
		}
	}
	return t, b, nil
}

// DecodeAll decodes values until b is exhausted.
func DecodeAll(b []byte) (Tuple, error) {
	var t Tuple
	for len(b) > 0 {
		v, rest, err := DecodeKey(b)
		if err != nil {
			return nil, err
		}
		t = append(t, v)
		b = rest
	}
	return t, nil
}

// PrefixEnd returns the smallest key greater than every key with prefix p, or
// nil if no such key exists (p is empty or all 0xFF).
func PrefixEnd(p []byte) []byte {
	end := append([]byte(nil), p...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

func appendEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == escByte {
			dst = append(dst, escByte, escEsc)
		} else {
			dst = append(dst, s[i])
		}
	}
	return append(dst, escByte, escTerm)
}

func readEscaped(b []byte) (string, []byte, error) {
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] != escByte {
			out = append(out, b[i])
			continue
		}
		if i+1 >= len(b) {
			break
		}
		switch b[i+1] {
		case escTerm:
			return string(out), b[i+2:], nil
		case escEsc:
			out = append(out, escByte)
			i++
		default:
			return "", nil, fmt.Errorf("value: decode: bad escape 0x%02x", b[i+1])
		}
	}
	return "", nil, errors.New("value: decode: unterminated string")
}

// floatKey maps IEEE-754 bits to an unsigned integer with the same order:
// flip all bits of negatives, flip only the sign bit of positives.
func floatKey(bits uint64) uint64 {
	if bits&signFlip != 0 {
		return ^bits
	}
	return bits | signFlip
}

func floatUnkey(u uint64) uint64 {
	if u&signFlip != 0 {
		return u &^ signFlip
	}
	return ^u
}
