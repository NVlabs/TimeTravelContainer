// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
