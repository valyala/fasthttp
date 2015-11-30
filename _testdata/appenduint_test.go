package main

import "testing"

func BenchmarkAppend1(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_b = AppendUint1(i)
	}
}

func BenchmarkAppend2(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_b = AppendUint2(i)
	}
}
