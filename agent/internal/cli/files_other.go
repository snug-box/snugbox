//go:build !linux

package cli

import (
	"errors"
	"os"
)

const noFollow = 0

var errLinuxOnly = errors.New("snugbox changes Linux systems only")

func saveFile(string, string, []byte) error { return errLinuxOnly }

func writeSystemFile(string, []byte, os.FileMode) error { return errLinuxOnly }

func createFile(string, []byte, os.FileMode) (bool, error) { return false, errLinuxOnly }

func privateDir(string) error { return errLinuxOnly }
