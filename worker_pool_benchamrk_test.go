package fasthttp

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

func genTestData(n int, percent float32) ([]*worker, time.Time) {
	currentTime := time.Now()
	workers := make([]*worker, 0, n)
	deltas := make([]int, 0, n)
	rand.Seed(currentTime.UnixNano())
	for i := 0; i < n; i++ {
		delta := rand.Intn(n)
		deltas = append(deltas, delta)
	}
	sort.Ints(deltas)

	for _, delta := range deltas {
		workers = append(workers, &worker{currentTime.Add(time.Duration(delta) * time.Second)})
	}
	index := int(percent * float32(n))
	criticalTime := workers[index].lastUseTime.Add(1 * time.Second)
	return workers, criticalTime
}

func binarySearch(workers []*worker, l, r int, criticalTime time.Time) int {
	var mid int
	for l <= r {
		mid = (l + r) / 2
		if criticalTime.After(workers[mid].lastUseTime) {
			l = mid + 1
		} else {
			r = mid - 1
		}
	}
	return r
}

func linearSearch(workers []*worker, n int, criticalTime time.Time) int {
	i := 0
	for i < n && criticalTime.After(workers[i].lastUseTime) {
		i++
	}
	return i
}

type worker struct {
	lastUseTime time.Time
}

func BenchmarkCleanWorkers1(b *testing.B) {
	workers, criticalTime := genTestData(10, 0.1)
	wLen := len(workers)
	rIndex := wLen - 1
	b.Run("10,10%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(100, 0.1)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("100,10%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(1000, 0.1)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("1000,10%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(10000, 0.1)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("10000,10%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})
}

func BenchmarkCleanWorkers2(b *testing.B) {
	workers, criticalTime := genTestData(10, 0.5)
	wLen := len(workers)
	rIndex := wLen - 1
	b.Run("10,50%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(100, 0.5)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("100,50%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(1000, 0.5)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("1000,50%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(10000, 0.5)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("10000,50%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})
}

func BenchmarkCleanWorkers3(b *testing.B) {
	workers, criticalTime := genTestData(10, 0.9)
	wLen := len(workers)
	rIndex := wLen - 1
	b.Run("10,90%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(100, 0.9)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("100,90%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(1000, 0.9)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("1000,90%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})

	workers, criticalTime = genTestData(10000, 0.9)
	wLen = len(workers)
	rIndex = wLen - 1
	b.Run("10000,90%", func(b *testing.B) {
		b.Run("binary search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				binarySearch(workers, 0, rIndex, criticalTime)
			}
		})
		b.Run("linear search", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				linearSearch(workers, wLen, criticalTime)
			}
		})
	})
}
