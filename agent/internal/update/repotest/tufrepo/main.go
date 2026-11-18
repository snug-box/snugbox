// Command tufrepo keeps a test update repository for snugbox self-update on
// a test machine. Its keys are test keys on disk; the real repository's
// root keys live on the owner's hardware keys.
//
//	tufrepo init DIR
//	tufrepo trust DIR URL OUTDIR             write root.json and repository.json for a test build
//	tufrepo publish DIR CHANNEL ARCH VERSION TARBALL
//	tufrepo tamper DIR CHANNEL ARCH          change a published release's bytes
//	tufrepo expire DIR                       sign a timestamp that has expired
//
// Serve DIR over HTTP, for example with python3 -m http.server, at URL.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"snugbox.dev/snugbox/agent/internal/update"
	"snugbox.dev/snugbox/agent/internal/update/repotest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tufrepo:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: tufrepo init|trust|publish|tamper|expire DIR and the verb's arguments")
	}
	verb, dir := args[0], args[1]
	if verb == "init" {
		_, err := repotest.Init(dir)
		return err
	}
	r, err := repotest.Open(dir)
	if err != nil {
		return err
	}
	switch {
	case verb == "trust" && len(args) == 4:
		root, err := r.Root()
		if err != nil {
			return err
		}
		url := strings.TrimSuffix(args[2], "/")
		repo, err := json.Marshal(update.Repository{MetadataURL: url + "/metadata/", TargetsURL: url + "/targets/"})
		if err != nil {
			return err
		}
		if err := os.MkdirAll(args[3], 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(args[3], "root.json"), root, 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(args[3], "repository.json"), repo, 0o644)
	case verb == "publish" && len(args) == 6:
		tarball, err := os.ReadFile(args[5])
		if err != nil {
			return err
		}
		return r.Publish(args[2], args[3], args[4], tarball)
	case verb == "tamper" && len(args) == 4:
		return r.Tamper(update.TargetName(args[2], args[3]))
	case verb == "expire" && len(args) == 2:
		return r.Expire()
	}
	return fmt.Errorf("can't %s with %d arguments", verb, len(args)-1)
}
