// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin && arm64

package time

// platformInitialClockParameters constructs an immediately usable clock from
// ARM's architected counter frequency and one bracketed Darwin reference
// sample. CNTFRQ_EL0 is the exact frequency of the CNTVCT_EL0 counter used by
// Rdtsc; periodic calibration remains responsible for correcting reference
// clock adjustments after startup.
func platformInitialClockParameters(clockID ClockID) (Parameters, bool) {
	sample, err := (syscallTSCReferenceClocks{}).Sample(clockID)
	if err != nil || sample.after < sample.before {
		return Parameters{}, false
	}
	frequency := getCNTFRQ()
	if frequency <= 0 {
		return Parameters{}, false
	}
	return Parameters{
		BaseCycles: sample.before + (sample.after-sample.before)/2,
		BaseRef:    sample.ref,
		Frequency:  uint64(frequency),
	}, true
}
