package main

import (
	"io"

	"github.com/BurntSushi/toml"
)

// tomlEncoder wraps the TOML encoder so main.go does not need the import.
func tomlEncoder(w io.Writer) *toml.Encoder { return toml.NewEncoder(w) }
