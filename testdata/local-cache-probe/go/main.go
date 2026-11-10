package main

import (
	"fmt"
	"github.com/google/uuid"
)

func main() {
	if uuid.MustParse("00000000-0000-0000-0000-000000000001").String() != "00000000-0000-0000-0000-000000000001" {
		panic("unexpected UUID")
	}
	fmt.Println("runscaler-local-cache")
}
