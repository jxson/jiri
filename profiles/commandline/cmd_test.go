// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package commandline

import (
	"v.io/jiri/jiri"
)

func Reset() {
	cmdList = newCmdList()
	cmdList.Runner = jiri.RunnerFunc(runList)
	cmdEnv = newCmdEnv()
	listFlags.ReaderFlagValues = nil
	envFlags.ReaderFlagValues = nil
}
