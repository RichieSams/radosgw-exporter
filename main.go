package main

import (
	"github.com/RichieSams/radosgw-exporter/pkg"
)

func main() {
	if log, err := pkg.RunServer(); err != nil {
		log.Fatal(err)
	}
}
