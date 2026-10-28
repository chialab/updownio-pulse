package main

import "os"

// Caught only so the wrapper survives Ctrl+C long enough to send the pulse.
func forwardedSignals() []os.Signal { return []os.Signal{os.Interrupt} }
