package main

import (
	"fmt"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/CircularBuffer"
)

func main() {
	buffer := CircularBuffer.NewCircularBuffer[byte](0, 12)

	n, err := buffer.Write([]byte("Hello World"))
	fmt.Printf("Wrote %d bytes, err %v\n", n, err)

	buf := make([]byte, 5)
	n, err = buffer.Read(buf)
	fmt.Printf("Read %d bytes, err %v, buf=%v\n", n, err, string(buf[:n]))

	n, err = buffer.Read(buf)
	fmt.Printf("Read %d bytes, err %v, buf=%v\n", n, err, string(buf[:n]))

	n, err = buffer.Read(buf)
	fmt.Printf("Read %d bytes, err %v, buf=%v\n", n, err, string(buf[:n]))

	n, err = buffer.Write([]byte("Hel World"))
	fmt.Printf("Wrote %d bytes, err %v\n", n, err)

	n, err = buffer.Read(buf)
	fmt.Printf("Read %d bytes, err %v, buf=%v\n", n, err, string(buf[:n]))
}
