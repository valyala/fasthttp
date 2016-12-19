// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zwrap_test

import (
	"time"

	"github.com/uber-go/zap"
	"github.com/uber-go/zap/zwrap"
)

func Example_standardize() {
	zapLogger := zap.New(zap.NewJSONEncoder(
		zap.NoTime(), // discard timestamps in tests
	))

	// Wrap our structured logger to mimic the standard library's log.Logger.
	// We also specify that we want all calls to the standard logger's Print
	// family of methods to log at zap's Warn level.
	stdLogger, err := zwrap.Standardize(zapLogger, zap.WarnLevel)
	if err != nil {
		panic(err.Error())
	}

	// The wrapped logger has the usual Print, Panic, and Fatal families of
	// methods.
	stdLogger.Printf("Encountered %d errors.", 0)

	// Output:
	// {"level":"warn","msg":"Encountered 0 errors."}
}

func Example_sample() {
	zapLogger := zap.New(zap.NewJSONEncoder(
		zap.NoTime(), // discard timestamps in tests
	))
	sampledLogger := zwrap.Sample(zapLogger, time.Second, 1, 100)

	for i := 1; i < 110; i++ {
		sampledLogger.With(zap.Int("n", i)).Error("Common failure.")
	}

	sampledLogger.Error("Unusual failure.")

	// Output:
	// {"level":"error","msg":"Common failure.","n":1}
	// {"level":"error","msg":"Common failure.","n":101}
	// {"level":"error","msg":"Unusual failure."}
}
