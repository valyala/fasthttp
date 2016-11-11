// Package stackless saves stack space for high number of concurrently
// running goroutines, which use writers from compress/* packages.
package stackless
