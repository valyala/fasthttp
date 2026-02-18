//go:build darwin

package listensocket

var NewSocketCloexec = newSocketCloexecOld
