// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// construct is an open IF or loop of the body, as SPIR-V's structured
// control flow has them: its merge block, and an IF's else block, which it
// always has, empty if TGSI's has no ELSE, or a loop's continue block.
type construct struct {
	loop bool
	// merge is the block after the construct.
	merge spirv.ID
	// other is an IF's else block, or a loop's continue block.
	other spirv.ID
	// inElse is whether an IF's ELSE began.
	inElse bool
	// header is a loop's header, which its continue block branches to.
	header spirv.ID
}

// flowContinues reports whether op handles a block that ended before it
// itself: a construct's ELSE or end, or END.
func (t *translator) flowContinues(op tgsi.Opcode) bool {
	switch op {
	case tgsi.OpELSE, tgsi.OpENDIF, tgsi.OpENDLOOP, tgsi.OpEND:
		return true
	}
	return false
}

// branch ends the current block with a branch to target, unless it ended.
func (t *translator) branch(target spirv.ID) {
	if !t.terminated {
		t.f.Do(spirv.OpBranch, uint32(target))
	}
	t.terminated = true
}

// begin begins block id.
func (t *translator) begin(id spirv.ID) {
	t.f.Begin(id)
	t.terminated = false
}

// innermostLoop returns the innermost open loop.
func (t *translator) innermostLoop() *construct {
	for i := len(t.flow) - 1; i >= 0; i-- {
		if t.flow[i].loop {
			return &t.flow[i]
		}
	}
	t.fail("BRK or CONT outside a loop")
	return nil
}

// flowInstruction translates a control flow instruction, and returns
// whether it is one.
func (t *translator) flowInstruction(inst *tgsi.Instruction) bool {
	switch inst.Opcode {
	case tgsi.OpIF, tgsi.OpUIF:
		var cond spirv.ID
		if inst.Opcode == tgsi.OpIF {
			x := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 0, Float)), 0)
			cond = t.f.Op(spirv.OpFUnordNotEqual, t.tBool, uint32(x), uint32(t.float32Const(0)))
		} else {
			x := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(t.src(inst, 0, Uint)), 0)
			cond = t.f.Op(spirv.OpINotEqual, t.tBool, uint32(x), uint32(t.m.ConstantUint(0)))
		}
		c := construct{merge: t.m.NewID(), other: t.m.NewID()}
		then := t.m.NewID()
		t.f.Do(spirv.OpSelectionMerge, uint32(c.merge), uint32(spirv.SelectionControlNone))
		t.f.Do(spirv.OpBranchConditional, uint32(cond), uint32(then), uint32(c.other))
		t.flow = append(t.flow, c)
		t.terminated = true
		t.begin(then)
	case tgsi.OpELSE:
		if len(t.flow) == 0 || t.flow[len(t.flow)-1].loop || t.flow[len(t.flow)-1].inElse {
			t.fail("ELSE outside an IF")
		}
		c := &t.flow[len(t.flow)-1]
		t.branch(c.merge)
		c.inElse = true
		t.begin(c.other)
	case tgsi.OpENDIF:
		if len(t.flow) == 0 || t.flow[len(t.flow)-1].loop {
			t.fail("ENDIF outside an IF")
		}
		c := t.flow[len(t.flow)-1]
		t.flow = t.flow[:len(t.flow)-1]
		t.branch(c.merge)
		if !c.inElse {
			t.begin(c.other)
			t.branch(c.merge)
		}
		t.begin(c.merge)
	case tgsi.OpBGNLOOP:
		c := construct{loop: true, merge: t.m.NewID(), other: t.m.NewID(), header: t.m.NewID()}
		body := t.m.NewID()
		t.branch(c.header)
		t.begin(c.header)
		t.f.Do(spirv.OpLoopMerge, uint32(c.merge), uint32(c.other), uint32(spirv.LoopControlNone))
		t.f.Do(spirv.OpBranch, uint32(body))
		t.terminated = true
		t.flow = append(t.flow, c)
		t.begin(body)
	case tgsi.OpENDLOOP:
		if len(t.flow) == 0 || !t.flow[len(t.flow)-1].loop {
			t.fail("ENDLOOP outside a loop")
		}
		c := t.flow[len(t.flow)-1]
		t.flow = t.flow[:len(t.flow)-1]
		t.branch(c.other)
		t.begin(c.other)
		t.branch(c.header)
		t.begin(c.merge)
	case tgsi.OpBRK:
		t.branch(t.innermostLoop().merge)
	case tgsi.OpCONT:
		t.branch(t.innermostLoop().other)
	case tgsi.OpKILL:
		t.kill()
	case tgsi.OpKILL_IF:
		// Kills if any channel is negative.
		lt := t.f.Op(spirv.OpFOrdLessThan, t.tVec4B, uint32(t.src(inst, 0, Float)), uint32(t.constVec4F(0, 0, 0, 0)))
		cond := t.f.Op(spirv.OpAny, t.tBool, uint32(lt))
		merge, kill := t.m.NewID(), t.m.NewID()
		t.f.Do(spirv.OpSelectionMerge, uint32(merge), uint32(spirv.SelectionControlNone))
		t.f.Do(spirv.OpBranchConditional, uint32(cond), uint32(kill), uint32(merge))
		t.terminated = true
		t.begin(kill)
		t.kill()
		t.begin(merge)
	case tgsi.OpRET:
		if !t.terminated {
			t.f.Do(spirv.OpReturn)
		}
		t.terminated = true
	default:
		return false
	}
	return true
}

// kill discards the fragment.
func (t *translator) kill() {
	if t.s.Processor != tgsi.ProcessorFragment {
		t.fail("KILL of %v", t.s.Processor)
	}
	t.kills = true
	if !t.terminated {
		t.f.Do(spirv.OpKill)
	}
	t.terminated = true
}
