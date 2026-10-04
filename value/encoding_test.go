package value

import (
	"bytes"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func randScalar(r *rand.Rand) Value {
	switch r.IntN(7) {
	case 0:
		return Null
	case 1:
		return Bool(r.IntN(2) == 0)
	case 2:
		switch r.IntN(4) {
		case 0:
			return Int(math.MinInt64 + r.Int64N(3))
		case 1:
			return Int(math.MaxInt64 - r.Int64N(3))
		default:
			return Int(r.Int64N(2001) - 1000)
		}
	case 3:
		special := []float64{0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), math.NaN(), -math.NaN(), math.SmallestNonzeroFloat64, -math.MaxFloat64}
		if r.IntN(4) == 0 {
			return Float(special[r.IntN(len(special))])
		}
		return Float(r.NormFloat64() * 1000)
	case 4:
		return String(randStr(r))
	case 5:
		return Bytes([]byte(randStr(r)))
	default:
		return Timestamp(time.Unix(0, r.Int64N(1<<62)-1<<61))
	}
}

// randStr favors short strings drawn from a tiny alphabet that includes the
// escape-relevant bytes, so prefixes and escapes collide often.
func randStr(r *rand.Rand) string {
	alphabet := []byte{0x00, 0x01, 0xFF, 'a', 'b'}
	n := r.IntN(5)
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

func randTuple(r *rand.Rand) Tuple {
	t := make(Tuple, r.IntN(4))
	for i := range t {
		t[i] = randScalar(r)
	}
	return t
}

func mustEnc(t *testing.T, tu Tuple) []byte {
	t.Helper()
	b, err := EncodeTuple(nil, tu)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

func TestEncodingPreservesOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 200000 {
		a, b := randTuple(r), randTuple(r)
		want := sign(CompareTuples(a, b))
		got := sign(bytes.Compare(mustEnc(t, a), mustEnc(t, b)))
		if want != got {
			t.Fatalf("order mismatch: %v vs %v: compare=%d bytes=%d", a, b, want, got)
		}
	}
}

func TestEncodingRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 50000 {
		tu := randTuple(r)
		enc := mustEnc(t, tu)
		got, err := DecodeAll(enc)
		if err != nil {
			t.Fatalf("decode %v: %v", tu, err)
		}
		if CompareTuples(got, tu) != 0 || len(got) != len(tu) {
			t.Fatalf("round trip: got %v want %v", got, tu)
		}
	}
}

func TestEncodingPrefix(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for range 50000 {
		tu := randTuple(r)
		n := 0
		if len(tu) > 0 {
			n = r.IntN(len(tu) + 1)
		}
		full, prefix := mustEnc(t, tu), mustEnc(t, tu[:n])
		if !bytes.HasPrefix(full, prefix) {
			t.Fatalf("enc(%v) is not a prefix of enc(%v)", tu[:n], tu)
		}
		// Every key with this tuple prefix sorts below PrefixEnd.
		if end := PrefixEnd(prefix); end != nil && bytes.Compare(full, end) >= 0 {
			t.Fatalf("enc(%v) >= PrefixEnd(enc(%v))", tu, tu[:n])
		}
	}
}

func TestDecodeTupleRest(t *testing.T) {
	enc := mustEnc(t, Tuple{String("a\x00b"), Int(-7), Float(1.5)})
	head, rest, err := DecodeTuple(enc, 2)
	if err != nil {
		t.Fatal(err)
	}
	if head[0].Str() != "a\x00b" || head[1].Int() != -7 {
		t.Fatalf("unexpected head %v", head)
	}
	tail, err := DecodeAll(rest)
	if err != nil || len(tail) != 1 || tail[0].Float() != 1.5 {
		t.Fatalf("unexpected tail %v %v", tail, err)
	}
}

func TestNotEncodable(t *testing.T) {
	if _, err := EncodeKey(nil, List(nil)); err == nil {
		t.Fatal("expected error for list")
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{1, 2}, []byte{1, 3}},
		{[]byte{1, 0xFF}, []byte{2}},
		{[]byte{0xFF, 0xFF}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("PrefixEnd(%x) = %x, want %x", c.in, got, c.want)
		}
	}
}
