// Copyright 2016 The Fuchsia Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

// This file contains helper functions related to running shell commands in tests.

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runCmd handles the boilerplate associated with running an exec.Cmd object.
// In particular, it handles wiring up the std{out,err} pipes, reading from them,
// and checking errors.  runCmd doesn't return error because errors are reported
// on the testing object.  runCmd returns the stdout and stderr as strings.
func runCmd(t *testing.T, cmd *exec.Cmd, failureExpected bool) (string, string) {
	// Make sure go is in the path.
	_, err := exec.LookPath(cmd.Path)
	if err != nil {
		t.Fatal(err)
	}

	// Wire up output pipes.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	// Using .Start() is required when fetching output from pipes.
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Reading from pipes is required before calling .Wait().
	outBytes, err := ioutil.ReadAll(stdout)
	if err != nil {
		t.Fatal(err)
	}
	errBytes, err := ioutil.ReadAll(stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(outBytes) > 0 || len(errBytes) > 0 {
		t.Logf("Command (%s) has output\nFull command: %v\n", cmd.Path, cmd.Args)
	}
	if len(outBytes) > 0 {
		t.Logf("Stdout:\n%s\n", string(outBytes))
	}
	if len(errBytes) > 0 {
		t.Logf("Stderr:\n%s\n", string(errBytes))
	}

	// Wait for it...
	if err = cmd.Wait(); err != nil {
		if !failureExpected {
			t.Fatal(err)
		}
	}

	return string(outBytes), string(errBytes)
}

// buildGoPkg runs `go build` with the given package, and puts the binary in the given buildDir.
func buildGoPkg(t *testing.T, pkg string, buildDir string) string {
	// Sanity checking: let's make sure the given buildDir exists and is a directory.
	finfo, err := os.Stat(buildDir)
	if err != nil {
		t.Fatal(err)
	}
	if !finfo.IsDir() {
		t.Fatal(fmt.Errorf("buildDir (%s) is not a directory\n", buildDir))
	}

	// In case you're wondering why we don't have to set GOPATH here, it's because it uses
	// the one from the environment in which we're running the overall `go test` command.
	buildJiriCmd := exec.Command("go", "build", "-o", filepath.Base(pkg), pkg)
	buildJiriCmd.Dir = buildDir
	runCmd(t, buildJiriCmd, false)
	return filepath.Join(buildDir, filepath.Base(pkg))
}
