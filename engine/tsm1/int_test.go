package tsm1_test

import (
	"math"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/influxdb/influxdb/tsdb/engine/tsm1"
)

func Test_Int64Encoder_NoValues(t *testing.T) {
	enc := tsm1.NewInt64Encoder()
	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec := tsm1.NewInt64Decoder(b)
	if dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}
}

func Test_Int64Encoder_One(t *testing.T) {
	enc := tsm1.NewInt64Encoder()
	v1 := int64(1)

	enc.Write(1)
	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec := tsm1.NewInt64Decoder(b)
	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v1 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v1)
	}
}

func Test_Int64Encoder_Two(t *testing.T) {
	enc := tsm1.NewInt64Encoder()
	var v1, v2 int64 = 1, 2

	enc.Write(v1)
	enc.Write(v2)

	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec := tsm1.NewInt64Decoder(b)
	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v1 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v1)
	}

	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v2 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v2)
	}
}

func Test_Int64Encoder_Negative(t *testing.T) {
	enc := tsm1.NewInt64Encoder()
	var v1, v2, v3 int64 = -2, 0, 1

	enc.Write(v1)
	enc.Write(v2)
	enc.Write(v3)

	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec := tsm1.NewInt64Decoder(b)
	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v1 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v1)
	}

	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v2 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v2)
	}

	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v3 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v3)
	}
}

func Test_Int64Encoder_Large_Range(t *testing.T) {
	enc := tsm1.NewInt64Encoder()
	var v1, v2 int64 = math.MinInt64, math.MaxInt64
	enc.Write(v1)
	enc.Write(v2)
	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec := tsm1.NewInt64Decoder(b)
	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v1 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v1)
	}

	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v2 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v2)
	}
}

func Test_Int64Encoder_Uncompressed(t *testing.T) {
	enc := tsm1.NewInt64Encoder()
	var v1, v2, v3 int64 = 0, 1, 1 << 60

	enc.Write(v1)
	enc.Write(v2)
	enc.Write(v3)

	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("expected error: %v", err)
	}

	// 1 byte header + 3 * 8 byte values
	if exp := 25; len(b) != exp {
		t.Fatalf("length mismatch: got %v, exp %v", len(b), exp)
	}

	dec := tsm1.NewInt64Decoder(b)
	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v1 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v1)
	}

	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v2 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v2)
	}

	if !dec.Next() {
		t.Fatalf("unexpected next value: got true, exp false")
	}

	if v3 != dec.Read() {
		t.Fatalf("read value mismatch: got %v, exp %v", dec.Read(), v3)
	}
}

func Test_Int64Encoder_NegativeUncompressed(t *testing.T) {
	values := []int64{
		-2352281900722994752, 1438442655375607923, -4110452567888190110,
		-1221292455668011702, -1941700286034261841, -2836753127140407751,
		1432686216250034552, 3663244026151507025, -3068113732684750258,
		-1949953187327444488, 3713374280993588804, 3226153669854871355,
		-2093273755080502606, 1006087192578600616, -2272122301622271655,
		2533238229511593671, -4450454445568858273, 2647789901083530435,
		2761419461769776844, -1324397441074946198, -680758138988210958,
		94468846694902125, -2394093124890745254, -2682139311758778198,
	}
	enc := tsm1.NewInt64Encoder()
	for _, v := range values {
		enc.Write(v)
	}

	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("expected error: %v", err)
	}

	dec := tsm1.NewInt64Decoder(b)

	i := 0
	for dec.Next() {
		if i > len(values) {
			t.Fatalf("read too many values: got %v, exp %v", i, len(values))
		}

		if values[i] != dec.Read() {
			t.Fatalf("read value %d mismatch: got %v, exp %v", i, dec.Read(), values[i])
		}
		i += 1
	}

	if i != len(values) {
		t.Fatalf("failed to read enough values: got %v, exp %v", i, len(values))
	}
}

func Test_Int64Encoder_AllNegative(t *testing.T) {
	enc := tsm1.NewInt64Encoder()
	values := []int64{
		-10, -5, -1,
	}

	for _, v := range values {
		enc.Write(v)
	}

	b, err := enc.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec := tsm1.NewInt64Decoder(b)
	i := 0
	for dec.Next() {
		if i > len(values) {
			t.Fatalf("read too many values: got %v, exp %v", i, len(values))
		}

		if values[i] != dec.Read() {
			t.Fatalf("read value %d mismatch: got %v, exp %v", i, dec.Read(), values[i])
		}
		i += 1
	}

	if i != len(values) {
		t.Fatalf("failed to read enough values: got %v, exp %v", i, len(values))
	}

}

func Test_Int64Encoder_Quick(t *testing.T) {
	quick.Check(func(values []int64) bool {
		// Write values to encoder.
		enc := tsm1.NewInt64Encoder()
		for _, v := range values {
			enc.Write(v)
		}

		// Retrieve encoded bytes from encoder.
		buf, err := enc.Bytes()
		if err != nil {
			t.Fatal(err)
		}

		// Read values out of decoder.
		got := make([]int64, 0, len(values))
		dec := tsm1.NewInt64Decoder(buf)
		for dec.Next() {
			if err := dec.Error(); err != nil {
				t.Fatal(err)
			}
			got = append(got, dec.Read())
		}

		// Verify that input and output values match.
		if !reflect.DeepEqual(values, got) {
			t.Fatalf("mismatch:\n\nexp=%+v\n\ngot=%+v\n\n", values, got)
		}

		return true
	}, nil)
}

func BenchmarkInt64Encoder(b *testing.B) {
	enc := tsm1.NewInt64Encoder()
	x := make([]int64, 1024)
	for i := 0; i < len(x); i++ {
		x[i] = int64(i)
		enc.Write(x[i])
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Bytes()
	}
}

type byteSetter interface {
	SetBytes(b []byte)
}

func BenchmarkInt64Decoder(b *testing.B) {
	x := make([]int64, 1024)
	enc := tsm1.NewInt64Encoder()
	for i := 0; i < len(x); i++ {
		x[i] = int64(i)
		enc.Write(x[i])
	}
	bytes, _ := enc.Bytes()

	b.ResetTimer()

	dec := tsm1.NewInt64Decoder(bytes)

	for i := 0; i < b.N; i++ {
		dec.(byteSetter).SetBytes(bytes)
		for dec.Next() {
		}
	}
}
