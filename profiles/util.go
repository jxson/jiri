// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package profiles

import (
	"archive/zip"
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"v.io/jiri/jiri"
	"v.io/jiri/runutil"
	"v.io/jiri/tool"
)

const (
	// TODO(cnicolaou): move these to runutil
	DefaultDirPerm  = os.FileMode(0755)
	DefaultFilePerm = os.FileMode(0644)
	targetDefValue  = "<runtime.GOARCH>-<runtime.GOOS>"
)

// RegisterTargetAndEnvFlags registers the commonly used --target and --env
// flags with the supplied FlagSet
func RegisterTargetAndEnvFlags(flags *flag.FlagSet, target *Target) {
	*target = DefaultTarget()
	flags.Var(target, "target", target.Usage())
	flags.Lookup("target").DefValue = targetDefValue
	flags.Var(&target.commandLineEnv, "env", target.commandLineEnv.Usage())
}

// RegisterProfilesFlag registers the --profiles flag
func RegisterProfilesFlag(flags *flag.FlagSet, profiles *string) {
	flags.StringVar(profiles, "profiles", "base,jiri", "a comma separated list of profiles to use")
}

// RegisterTargetFlag registers the commonly used --target flag with
// the supplied FlagSet.
func RegisterTargetFlag(flags *flag.FlagSet, target *Target) {
	*target = DefaultTarget()
	flags.Var(target, "target", target.Usage())
	flags.Lookup("target").DefValue = targetDefValue
}

// AtomicAction performs an action 'atomically' by keeping track of successfully
// completed actions in the supplied completion log and re-running them if they
// are not successfully logged therein after deleting the entire contents of the
// dir parameter. Consequently it does not make sense to apply AtomicAction to
// the same directory in sequence.
// TODO(cnicolaou): move this to runutil
func AtomicAction(jirix *jiri.X, installFn func() error, dir, message string) error {
	atomicFn := func() error {
		completionLogPath := filepath.Join(dir, ".complete")
		s := jirix.NewSeq()
		if dir != "" {
			if exists, _ := s.DirectoryExists(dir); exists {
				// If the dir exists but the completionLogPath doesn't, then it
				// means the previous action didn't finish.
				// Remove the dir so we can perform the action again.
				if exists, _ := s.FileExists(completionLogPath); !exists {
					s.RemoveAll(dir).Done()
				} else {
					if jirix.Verbose() {
						fmt.Fprintf(jirix.Stdout(), "AtomicAction: %s already completed in %s\n", message, dir)
					}
					return nil
				}
			}
		}
		if err := installFn(); err != nil {
			if dir != "" {
				s.RemoveAll(dir).Done()
			}
			return err
		}
		return s.WriteFile(completionLogPath, []byte("completed"), DefaultFilePerm).Done()
	}
	return jirix.NewSeq().Call(atomicFn, message).Done()
}

func brewList(jirix *jiri.X) (map[string]bool, error) {
	var out bytes.Buffer
	err := jirix.NewSeq().Capture(&out, &out).Last("brew", "list")
	if err != nil || tool.VerboseFlag {
		fmt.Fprintf(jirix.Stdout(), "%s", out.String())
	}
	scanner := bufio.NewScanner(&out)
	pkgs := map[string]bool{}
	for scanner.Scan() {
		pkgs[scanner.Text()] = true
	}
	return pkgs, err
}

// InstallPackages identifies the packages that need to be installed
// and installs them using the OS-specific package manager.
// TODO(cnicolaou): move these to runutil
func InstallPackages(jirix *jiri.X, pkgs []string) error {
	installDepsFn := func() error {
		s := jirix.NewSeq()
		switch runtime.GOOS {
		case "linux":
			if runutil.IsFNLHost() {
				fmt.Fprintf(jirix.Stdout(), "skipping installation of %v on FNL host", pkgs)
				fmt.Fprintf(jirix.Stdout(), "success\n")
				break
			}
			installPkgs := []string{}
			for _, pkg := range pkgs {
				if err := s.Last("dpkg", "-L", pkg); err != nil {
					installPkgs = append(installPkgs, pkg)
				}
			}
			if len(installPkgs) > 0 {
				args := append([]string{"apt-get", "install", "-y"}, installPkgs...)
				fmt.Fprintf(jirix.Stdout(), "Running: sudo %s: ", strings.Join(args, " "))
				if err := s.Last("sudo", args...); err != nil {
					fmt.Fprintf(jirix.Stdout(), "%v\n", err)
					return err
				}
				fmt.Fprintf(jirix.Stdout(), "success\n")
			}
		case "darwin":
			installPkgs := []string{}
			installedPkgs, err := brewList(jirix)
			if err != nil {
				return err
			}
			for _, pkg := range pkgs {
				if !installedPkgs[pkg] {
					installPkgs = append(installPkgs, pkg)
				}
			}
			if len(installPkgs) > 0 {
				args := append([]string{"install"}, installPkgs...)
				fmt.Fprintf(jirix.Stdout(), "Running: brew %s: ", strings.Join(args, " "))
				if err := s.Last("brew", args...); err != nil {
					fmt.Fprintf(jirix.Stdout(), "%v\n", err)
					return err
				}
				fmt.Fprintf(jirix.Stdout(), "success\n")
			}
		}
		return nil
	}
	return jirix.NewSeq().Call(installDepsFn, "Install dependencies: "+strings.Join(pkgs, ",")).Done()
}

// Fetch downloads the specified url and saves it to dst.
// TODO(nlacasse, cnicoloau): Move this to a package for profile-implementors
// so it does not pollute the profile package namespace.
// TODO(cnicolaou): move these to runutil
func Fetch(jirix *jiri.X, dst, url string) error {
	s := jirix.NewSeq()
	s.Output([]string{"fetching " + url})
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("got non-200 status code while getting %v: %v", url, resp.StatusCode)
	}
	file, err := s.Create(dst)
	if err != nil {
		return err
	}
	_, err = s.Copy(file, resp.Body)
	return err
}

// Unzip unzips the file in srcFile and puts resulting files in directory dstDir.
// TODO(nlacasse, cnicoloau): Move this to a package for profile-implementors
// so it does not pollute the profile package namespace.
// TODO(cnicolaou): move these to runutil
func Unzip(jirix *jiri.X, srcFile, dstDir string) error {
	r, err := zip.OpenReader(srcFile)
	if err != nil {
		return err
	}
	defer r.Close()

	unzipFn := func(zFile *zip.File) error {
		rc, err := zFile.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		s := jirix.NewSeq()
		fileDst := filepath.Join(dstDir, zFile.Name)
		if zFile.FileInfo().IsDir() {
			return s.MkdirAll(fileDst, zFile.Mode()).Done()
		}

		// Make sure the parent directory exists.  Note that sometimes files
		// can appear in a zip file before their directory.
		if err := s.MkdirAll(filepath.Dir(fileDst), zFile.Mode()).Done(); err != nil {
			return err
		}

		file, err := s.OpenFile(fileDst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, zFile.Mode())
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = s.Copy(file, rc)
		return err
	}
	s := jirix.NewSeq()
	s.Output([]string{"unzipping " + srcFile})
	for _, zFile := range r.File {
		s.Output([]string{"extracting " + zFile.Name})
		s.Call(func() error { return unzipFn(zFile) }, "unzipFn(%s)", zFile.Name)
	}
	return s.Done()
}
