package source

import (
	"fmt"
	"testing"
)

func BenchmarkReduce(b *testing.B) {
	sizes := []int{10, 100, 1000, 10000}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			lines := make([]DecisionRecord, size)
			for i := 0; i < size; i++ {
				lines[i] = DecisionRecord{ID: fmt.Sprintf("id-%d", i)}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				reduce(lines)
			}
		})
	}
}
