package main

import (
	"mihomo-manager/internal/manager"
	"os"
)

func main() {
	os.Exit(manager.Run(os.Args[1:]))
}
