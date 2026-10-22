// SPDX-FileCopyrightText: Copyright 2008 VMware, Inc.
// SPDX-License-Identifier: MIT

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tgsi

// The opcodes, as Mesa 26.2.4's tgsi_info_opcodes.h numbers and describes
// them: their names, their numbers of destination and source registers, how
// their results fill the destination's channels, and what they are.

// Opcode is an instruction's opcode.
type Opcode uint16

// The opcodes.
const (
	OpARL             Opcode = 0
	OpMOV             Opcode = 1
	OpLIT             Opcode = 2
	OpRCP             Opcode = 3
	OpRSQ             Opcode = 4
	OpEXP             Opcode = 5
	OpLOG             Opcode = 6
	OpMUL             Opcode = 7
	OpADD             Opcode = 8
	OpDP3             Opcode = 9
	OpDP4             Opcode = 10
	OpDST             Opcode = 11
	OpMIN             Opcode = 12
	OpMAX             Opcode = 13
	OpSLT             Opcode = 14
	OpSGE             Opcode = 15
	OpMAD             Opcode = 16
	OpLRP             Opcode = 18
	OpFMA             Opcode = 19
	OpSQRT            Opcode = 20
	OpLDEXP           Opcode = 21
	OpF2U64           Opcode = 22
	OpF2I64           Opcode = 23
	OpFRC             Opcode = 24
	OpFLR             Opcode = 26
	OpROUND           Opcode = 27
	OpEX2             Opcode = 28
	OpLG2             Opcode = 29
	OpPOW             Opcode = 30
	OpDEMOTE          Opcode = 31
	OpU2I64           Opcode = 32
	OpCLOCK           Opcode = 33
	OpI2I64           Opcode = 34
	OpREAD_HELPER     Opcode = 35
	OpCOS             Opcode = 36
	OpDDX             Opcode = 37
	OpDDY             Opcode = 38
	OpKILL            Opcode = 39
	OpPK2H            Opcode = 40
	OpPK2US           Opcode = 41
	OpPK4B            Opcode = 42
	OpPK4UB           Opcode = 43
	OpD2U64           Opcode = 44
	OpSEQ             Opcode = 45
	OpD2I64           Opcode = 46
	OpSGT             Opcode = 47
	OpSIN             Opcode = 48
	OpSLE             Opcode = 49
	OpSNE             Opcode = 50
	OpU642D           Opcode = 51
	OpTEX             Opcode = 52
	OpTXD             Opcode = 53
	OpTXP             Opcode = 54
	OpUP2H            Opcode = 55
	OpUP2US           Opcode = 56
	OpUP4B            Opcode = 57
	OpUP4UB           Opcode = 58
	OpU642F           Opcode = 59
	OpI642F           Opcode = 60
	OpARR             Opcode = 61
	OpI642D           Opcode = 62
	OpCAL             Opcode = 63
	OpRET             Opcode = 64
	OpSSG             Opcode = 65
	OpCMP             Opcode = 66
	OpTXB             Opcode = 68
	OpFBFETCH         Opcode = 69
	OpDIV             Opcode = 70
	OpDP2             Opcode = 71
	OpTXL             Opcode = 72
	OpBRK             Opcode = 73
	OpIF              Opcode = 74
	OpUIF             Opcode = 75
	OpREAD_INVOC      Opcode = 76
	OpELSE            Opcode = 77
	OpENDIF           Opcode = 78
	OpDDX_FINE        Opcode = 79
	OpDDY_FINE        Opcode = 80
	OpCEIL            Opcode = 83
	OpI2F             Opcode = 84
	OpNOT             Opcode = 85
	OpTRUNC           Opcode = 86
	OpSHL             Opcode = 87
	OpBALLOT          Opcode = 88
	OpAND             Opcode = 89
	OpOR              Opcode = 90
	OpMOD             Opcode = 91
	OpXOR             Opcode = 92
	OpTXF             Opcode = 94
	OpTXQ             Opcode = 95
	OpCONT            Opcode = 96
	OpEMIT            Opcode = 97
	OpENDPRIM         Opcode = 98
	OpBGNLOOP         Opcode = 99
	OpBGNSUB          Opcode = 100
	OpENDLOOP         Opcode = 101
	OpENDSUB          Opcode = 102
	OpATOMFADD        Opcode = 103
	OpTXQS            Opcode = 104
	OpRESQ            Opcode = 105
	OpREAD_FIRST      Opcode = 106
	OpNOP             Opcode = 107
	OpFSEQ            Opcode = 108
	OpFSGE            Opcode = 109
	OpFSLT            Opcode = 110
	OpFSNE            Opcode = 111
	OpMEMBAR          Opcode = 112
	OpKILL_IF         Opcode = 116
	OpEND             Opcode = 117
	OpDFMA            Opcode = 118
	OpF2I             Opcode = 119
	OpIDIV            Opcode = 120
	OpIMAX            Opcode = 121
	OpIMIN            Opcode = 122
	OpINEG            Opcode = 123
	OpISGE            Opcode = 124
	OpISHR            Opcode = 125
	OpISLT            Opcode = 126
	OpF2U             Opcode = 127
	OpU2F             Opcode = 128
	OpUADD            Opcode = 129
	OpUDIV            Opcode = 130
	OpUMAD            Opcode = 131
	OpUMAX            Opcode = 132
	OpUMIN            Opcode = 133
	OpUMOD            Opcode = 134
	OpUMUL            Opcode = 135
	OpUSEQ            Opcode = 136
	OpUSGE            Opcode = 137
	OpUSHR            Opcode = 138
	OpUSLT            Opcode = 139
	OpUSNE            Opcode = 140
	OpSWITCH          Opcode = 141
	OpCASE            Opcode = 142
	OpDEFAULT         Opcode = 143
	OpENDSWITCH       Opcode = 144
	OpSAMPLE          Opcode = 145
	OpSAMPLE_I        Opcode = 146
	OpSAMPLE_I_MS     Opcode = 147
	OpSAMPLE_B        Opcode = 148
	OpSAMPLE_C        Opcode = 149
	OpSAMPLE_C_LZ     Opcode = 150
	OpSAMPLE_D        Opcode = 151
	OpSAMPLE_L        Opcode = 152
	OpGATHER4         Opcode = 153
	OpSVIEWINFO       Opcode = 154
	OpSAMPLE_POS      Opcode = 155
	OpSAMPLE_INFO     Opcode = 156
	OpUARL            Opcode = 157
	OpUCMP            Opcode = 158
	OpIABS            Opcode = 159
	OpISSG            Opcode = 160
	OpLOAD            Opcode = 161
	OpSTORE           Opcode = 162
	OpIMG2HND         Opcode = 163
	OpSAMP2HND        Opcode = 164
	OpBARRIER         Opcode = 166
	OpATOMUADD        Opcode = 167
	OpATOMXCHG        Opcode = 168
	OpATOMCAS         Opcode = 169
	OpATOMAND         Opcode = 170
	OpATOMOR          Opcode = 171
	OpATOMXOR         Opcode = 172
	OpATOMUMIN        Opcode = 173
	OpATOMUMAX        Opcode = 174
	OpATOMIMIN        Opcode = 175
	OpATOMIMAX        Opcode = 176
	OpTEX2            Opcode = 177
	OpTXB2            Opcode = 178
	OpTXL2            Opcode = 179
	OpIMUL_HI         Opcode = 180
	OpUMUL_HI         Opcode = 181
	OpTG4             Opcode = 182
	OpLODQ            Opcode = 183
	OpIBFE            Opcode = 184
	OpUBFE            Opcode = 185
	OpBFI             Opcode = 186
	OpBREV            Opcode = 187
	OpPOPC            Opcode = 188
	OpLSB             Opcode = 189
	OpIMSB            Opcode = 190
	OpUMSB            Opcode = 191
	OpINTERP_CENTROID Opcode = 192
	OpINTERP_SAMPLE   Opcode = 193
	OpINTERP_OFFSET   Opcode = 194
	OpF2D             Opcode = 195
	OpD2F             Opcode = 196
	OpDABS            Opcode = 197
	OpDNEG            Opcode = 198
	OpDADD            Opcode = 199
	OpDMUL            Opcode = 200
	OpDMAX            Opcode = 201
	OpDMIN            Opcode = 202
	OpDSLT            Opcode = 203
	OpDSGE            Opcode = 204
	OpDSEQ            Opcode = 205
	OpDSNE            Opcode = 206
	OpDRCP            Opcode = 207
	OpDSQRT           Opcode = 208
	OpDMAD            Opcode = 209
	OpDFRAC           Opcode = 210
	OpDLDEXP          Opcode = 211
	OpD2I             Opcode = 213
	OpI2D             Opcode = 214
	OpD2U             Opcode = 215
	OpU2D             Opcode = 216
	OpDRSQ            Opcode = 217
	OpDTRUNC          Opcode = 218
	OpDCEIL           Opcode = 219
	OpDFLR            Opcode = 220
	OpDROUND          Opcode = 221
	OpDSSG            Opcode = 222
	OpVOTE_ANY        Opcode = 223
	OpVOTE_ALL        Opcode = 224
	OpVOTE_EQ         Opcode = 225
	OpU64SEQ          Opcode = 226
	OpU64SNE          Opcode = 227
	OpI64SLT          Opcode = 228
	OpU64SLT          Opcode = 229
	OpI64SGE          Opcode = 230
	OpU64SGE          Opcode = 231
	OpI64MIN          Opcode = 232
	OpU64MIN          Opcode = 233
	OpI64MAX          Opcode = 234
	OpU64MAX          Opcode = 235
	OpI64ABS          Opcode = 236
	OpI64SSG          Opcode = 237
	OpI64NEG          Opcode = 238
	OpU64ADD          Opcode = 239
	OpU64MUL          Opcode = 240
	OpU64SHL          Opcode = 241
	OpI64SHR          Opcode = 242
	OpU64SHR          Opcode = 243
	OpI64DIV          Opcode = 244
	OpU64DIV          Opcode = 245
	OpI64MOD          Opcode = 246
	OpU64MOD          Opcode = 247
	OpDDIV            Opcode = 248
	OpLOD             Opcode = 249
	OpATOMINC_WRAP    Opcode = 250
	OpATOMDEC_WRAP    Opcode = 251

	// NumOpcodes is one past the last opcode.
	NumOpcodes = 252
)

// OutputMode is how an instruction's result fills its destination's
// channels.
type OutputMode uint8

// The output modes.
const (
	// OutputNone: the instruction writes no register.
	OutputNone OutputMode = iota
	// OutputRepl: one scalar result, replicated to every channel written.
	OutputRepl
	// OutputChan: each channel's result of its own, computed from the
	// source's channels as a whole.
	OutputChan
	// OutputComp: componentwise, each channel's result of the sources' same
	// channel.
	OutputComp
	// OutputOther: of the instruction's own rule.
	OutputOther
)

// OpcodeInfo describes an opcode.
type OpcodeInfo struct {
	Name     string
	NumDst   int
	NumSrc   int
	Output   OutputMode
	IsTex    bool
	IsStore  bool
	IsBranch bool
	// PreDedent and PostIndent are the opcode's effect on the indentation
	// of the instructions after it, as Mesa prints them.
	PreDedent  bool
	PostIndent bool
}

var opcodeInfos = [NumOpcodes]OpcodeInfo{
	OpARL:             {Name: "ARL", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpMOV:             {Name: "MOV", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpLIT:             {Name: "LIT", NumDst: 1, NumSrc: 1, Output: OutputChan},
	OpRCP:             {Name: "RCP", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpRSQ:             {Name: "RSQ", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpEXP:             {Name: "EXP", NumDst: 1, NumSrc: 1, Output: OutputChan},
	OpLOG:             {Name: "LOG", NumDst: 1, NumSrc: 1, Output: OutputChan},
	OpMUL:             {Name: "MUL", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpADD:             {Name: "ADD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDP3:             {Name: "DP3", NumDst: 1, NumSrc: 2, Output: OutputRepl},
	OpDP4:             {Name: "DP4", NumDst: 1, NumSrc: 2, Output: OutputRepl},
	OpDST:             {Name: "DST", NumDst: 1, NumSrc: 2, Output: OutputChan},
	OpMIN:             {Name: "MIN", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpMAX:             {Name: "MAX", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpSLT:             {Name: "SLT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpSGE:             {Name: "SGE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpMAD:             {Name: "MAD", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpLRP:             {Name: "LRP", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpFMA:             {Name: "FMA", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpSQRT:            {Name: "SQRT", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpLDEXP:           {Name: "LDEXP", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpF2U64:           {Name: "F2U64", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpF2I64:           {Name: "F2I64", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpFRC:             {Name: "FRC", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpFLR:             {Name: "FLR", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpROUND:           {Name: "ROUND", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpEX2:             {Name: "EX2", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpLG2:             {Name: "LG2", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpPOW:             {Name: "POW", NumDst: 1, NumSrc: 2, Output: OutputRepl},
	OpDEMOTE:          {Name: "DEMOTE", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpU2I64:           {Name: "U2I64", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpCLOCK:           {Name: "CLOCK", NumDst: 1, NumSrc: 0, Output: OutputOther},
	OpI2I64:           {Name: "I2I64", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpREAD_HELPER:     {Name: "READ_HELPER", NumDst: 1, NumSrc: 0, Output: OutputComp},
	OpCOS:             {Name: "COS", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpDDX:             {Name: "DDX", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDDY:             {Name: "DDY", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpKILL:            {Name: "KILL", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpPK2H:            {Name: "PK2H", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpPK2US:           {Name: "PK2US", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpPK4B:            {Name: "PK4B", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpPK4UB:           {Name: "PK4UB", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpD2U64:           {Name: "D2U64", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpSEQ:             {Name: "SEQ", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpD2I64:           {Name: "D2I64", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpSGT:             {Name: "SGT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpSIN:             {Name: "SIN", NumDst: 1, NumSrc: 1, Output: OutputRepl},
	OpSLE:             {Name: "SLE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpSNE:             {Name: "SNE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU642D:           {Name: "U642D", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpTEX:             {Name: "TEX", NumDst: 1, NumSrc: 2, Output: OutputOther, IsTex: true},
	OpTXD:             {Name: "TXD", NumDst: 1, NumSrc: 4, Output: OutputOther, IsTex: true},
	OpTXP:             {Name: "TXP", NumDst: 1, NumSrc: 2, Output: OutputOther, IsTex: true},
	OpUP2H:            {Name: "UP2H", NumDst: 1, NumSrc: 1, Output: OutputChan},
	OpUP2US:           {Name: "UP2US", NumDst: 1, NumSrc: 1, Output: OutputChan},
	OpUP4B:            {Name: "UP4B", NumDst: 1, NumSrc: 1, Output: OutputChan},
	OpUP4UB:           {Name: "UP4UB", NumDst: 1, NumSrc: 1, Output: OutputChan},
	OpU642F:           {Name: "U642F", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpI642F:           {Name: "I642F", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpARR:             {Name: "ARR", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpI642D:           {Name: "I642D", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpCAL:             {Name: "CAL", NumDst: 0, NumSrc: 0, Output: OutputNone, IsBranch: true},
	OpRET:             {Name: "RET", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpSSG:             {Name: "SSG", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpCMP:             {Name: "CMP", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpTXB:             {Name: "TXB", NumDst: 1, NumSrc: 2, Output: OutputOther, IsTex: true},
	OpFBFETCH:         {Name: "FBFETCH", NumDst: 1, NumSrc: 1, Output: OutputOther},
	OpDIV:             {Name: "DIV", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDP2:             {Name: "DP2", NumDst: 1, NumSrc: 2, Output: OutputRepl},
	OpTXL:             {Name: "TXL", NumDst: 1, NumSrc: 2, Output: OutputOther, IsTex: true},
	OpBRK:             {Name: "BRK", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpIF:              {Name: "IF", NumDst: 0, NumSrc: 1, Output: OutputNone, IsBranch: true, PostIndent: true},
	OpUIF:             {Name: "UIF", NumDst: 0, NumSrc: 1, Output: OutputNone, IsBranch: true, PostIndent: true},
	OpREAD_INVOC:      {Name: "READ_INVOC", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpELSE:            {Name: "ELSE", NumDst: 0, NumSrc: 0, Output: OutputNone, IsBranch: true, PreDedent: true, PostIndent: true},
	OpENDIF:           {Name: "ENDIF", NumDst: 0, NumSrc: 0, Output: OutputNone, PreDedent: true},
	OpDDX_FINE:        {Name: "DDX_FINE", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDDY_FINE:        {Name: "DDY_FINE", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpCEIL:            {Name: "CEIL", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpI2F:             {Name: "I2F", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpNOT:             {Name: "NOT", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpTRUNC:           {Name: "TRUNC", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpSHL:             {Name: "SHL", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpBALLOT:          {Name: "BALLOT", NumDst: 1, NumSrc: 1, Output: OutputOther},
	OpAND:             {Name: "AND", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpOR:              {Name: "OR", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpMOD:             {Name: "MOD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpXOR:             {Name: "XOR", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpTXF:             {Name: "TXF", NumDst: 1, NumSrc: 2, Output: OutputOther, IsTex: true},
	OpTXQ:             {Name: "TXQ", NumDst: 1, NumSrc: 2, Output: OutputOther, IsTex: true},
	OpCONT:            {Name: "CONT", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpEMIT:            {Name: "EMIT", NumDst: 0, NumSrc: 1, Output: OutputNone},
	OpENDPRIM:         {Name: "ENDPRIM", NumDst: 0, NumSrc: 1, Output: OutputNone},
	OpBGNLOOP:         {Name: "BGNLOOP", NumDst: 0, NumSrc: 0, Output: OutputNone, IsBranch: true, PostIndent: true},
	OpBGNSUB:          {Name: "BGNSUB", NumDst: 0, NumSrc: 0, Output: OutputNone, PostIndent: true},
	OpENDLOOP:         {Name: "ENDLOOP", NumDst: 0, NumSrc: 0, Output: OutputNone, IsBranch: true, PreDedent: true},
	OpENDSUB:          {Name: "ENDSUB", NumDst: 0, NumSrc: 0, Output: OutputNone, PreDedent: true},
	OpATOMFADD:        {Name: "ATOMFADD", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpTXQS:            {Name: "TXQS", NumDst: 1, NumSrc: 1, Output: OutputOther, IsTex: true},
	OpRESQ:            {Name: "RESQ", NumDst: 1, NumSrc: 1, Output: OutputOther},
	OpREAD_FIRST:      {Name: "READ_FIRST", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpNOP:             {Name: "NOP", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpFSEQ:            {Name: "FSEQ", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpFSGE:            {Name: "FSGE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpFSLT:            {Name: "FSLT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpFSNE:            {Name: "FSNE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpMEMBAR:          {Name: "MEMBAR", NumDst: 0, NumSrc: 1, Output: OutputOther},
	OpKILL_IF:         {Name: "KILL_IF", NumDst: 0, NumSrc: 1, Output: OutputNone},
	OpEND:             {Name: "END", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpDFMA:            {Name: "DFMA", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpF2I:             {Name: "F2I", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpIDIV:            {Name: "IDIV", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpIMAX:            {Name: "IMAX", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpIMIN:            {Name: "IMIN", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpINEG:            {Name: "INEG", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpISGE:            {Name: "ISGE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpISHR:            {Name: "ISHR", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpISLT:            {Name: "ISLT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpF2U:             {Name: "F2U", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpU2F:             {Name: "U2F", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpUADD:            {Name: "UADD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUDIV:            {Name: "UDIV", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUMAD:            {Name: "UMAD", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpUMAX:            {Name: "UMAX", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUMIN:            {Name: "UMIN", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUMOD:            {Name: "UMOD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUMUL:            {Name: "UMUL", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUSEQ:            {Name: "USEQ", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUSGE:            {Name: "USGE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUSHR:            {Name: "USHR", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUSLT:            {Name: "USLT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUSNE:            {Name: "USNE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpSWITCH:          {Name: "SWITCH", NumDst: 0, NumSrc: 1, Output: OutputNone},
	OpCASE:            {Name: "CASE", NumDst: 0, NumSrc: 1, Output: OutputNone},
	OpDEFAULT:         {Name: "DEFAULT", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpENDSWITCH:       {Name: "ENDSWITCH", NumDst: 0, NumSrc: 0, Output: OutputNone},
	OpSAMPLE:          {Name: "SAMPLE", NumDst: 1, NumSrc: 3, Output: OutputOther},
	OpSAMPLE_I:        {Name: "SAMPLE_I", NumDst: 1, NumSrc: 2, Output: OutputOther},
	OpSAMPLE_I_MS:     {Name: "SAMPLE_I_MS", NumDst: 1, NumSrc: 3, Output: OutputOther},
	OpSAMPLE_B:        {Name: "SAMPLE_B", NumDst: 1, NumSrc: 4, Output: OutputOther},
	OpSAMPLE_C:        {Name: "SAMPLE_C", NumDst: 1, NumSrc: 4, Output: OutputOther},
	OpSAMPLE_C_LZ:     {Name: "SAMPLE_C_LZ", NumDst: 1, NumSrc: 4, Output: OutputOther},
	OpSAMPLE_D:        {Name: "SAMPLE_D", NumDst: 1, NumSrc: 5, Output: OutputOther},
	OpSAMPLE_L:        {Name: "SAMPLE_L", NumDst: 1, NumSrc: 4, Output: OutputOther},
	OpGATHER4:         {Name: "GATHER4", NumDst: 1, NumSrc: 3, Output: OutputOther},
	OpSVIEWINFO:       {Name: "SVIEWINFO", NumDst: 1, NumSrc: 2, Output: OutputOther},
	OpSAMPLE_POS:      {Name: "SAMPLE_POS", NumDst: 1, NumSrc: 2, Output: OutputOther},
	OpSAMPLE_INFO:     {Name: "SAMPLE_INFO", NumDst: 1, NumSrc: 2, Output: OutputOther},
	OpUARL:            {Name: "UARL", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpUCMP:            {Name: "UCMP", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpIABS:            {Name: "IABS", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpISSG:            {Name: "ISSG", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpLOAD:            {Name: "LOAD", NumDst: 1, NumSrc: 2, Output: OutputOther},
	OpSTORE:           {Name: "STORE", NumDst: 1, NumSrc: 2, Output: OutputOther, IsStore: true},
	OpIMG2HND:         {Name: "IMG2HND", NumDst: 1, NumSrc: 1, Output: OutputOther},
	OpSAMP2HND:        {Name: "SAMP2HND", NumDst: 1, NumSrc: 1, Output: OutputOther, IsTex: true},
	OpBARRIER:         {Name: "BARRIER", NumDst: 0, NumSrc: 0, Output: OutputOther},
	OpATOMUADD:        {Name: "ATOMUADD", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMXCHG:        {Name: "ATOMXCHG", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMCAS:         {Name: "ATOMCAS", NumDst: 1, NumSrc: 4, Output: OutputOther, IsStore: true},
	OpATOMAND:         {Name: "ATOMAND", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMOR:          {Name: "ATOMOR", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMXOR:         {Name: "ATOMXOR", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMUMIN:        {Name: "ATOMUMIN", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMUMAX:        {Name: "ATOMUMAX", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMIMIN:        {Name: "ATOMIMIN", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMIMAX:        {Name: "ATOMIMAX", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpTEX2:            {Name: "TEX2", NumDst: 1, NumSrc: 3, Output: OutputOther, IsTex: true},
	OpTXB2:            {Name: "TXB2", NumDst: 1, NumSrc: 3, Output: OutputOther, IsTex: true},
	OpTXL2:            {Name: "TXL2", NumDst: 1, NumSrc: 3, Output: OutputOther, IsTex: true},
	OpIMUL_HI:         {Name: "IMUL_HI", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpUMUL_HI:         {Name: "UMUL_HI", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpTG4:             {Name: "TG4", NumDst: 1, NumSrc: 3, Output: OutputOther, IsTex: true},
	OpLODQ:            {Name: "LODQ", NumDst: 1, NumSrc: 2, Output: OutputOther, IsTex: true},
	OpIBFE:            {Name: "IBFE", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpUBFE:            {Name: "UBFE", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpBFI:             {Name: "BFI", NumDst: 1, NumSrc: 4, Output: OutputComp},
	OpBREV:            {Name: "BREV", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpPOPC:            {Name: "POPC", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpLSB:             {Name: "LSB", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpIMSB:            {Name: "IMSB", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpUMSB:            {Name: "UMSB", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpINTERP_CENTROID: {Name: "INTERP_CENTROID", NumDst: 1, NumSrc: 1, Output: OutputOther},
	OpINTERP_SAMPLE:   {Name: "INTERP_SAMPLE", NumDst: 1, NumSrc: 2, Output: OutputOther},
	OpINTERP_OFFSET:   {Name: "INTERP_OFFSET", NumDst: 1, NumSrc: 2, Output: OutputOther},
	OpF2D:             {Name: "F2D", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpD2F:             {Name: "D2F", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDABS:            {Name: "DABS", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDNEG:            {Name: "DNEG", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDADD:            {Name: "DADD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDMUL:            {Name: "DMUL", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDMAX:            {Name: "DMAX", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDMIN:            {Name: "DMIN", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDSLT:            {Name: "DSLT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDSGE:            {Name: "DSGE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDSEQ:            {Name: "DSEQ", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDSNE:            {Name: "DSNE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDRCP:            {Name: "DRCP", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDSQRT:           {Name: "DSQRT", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDMAD:            {Name: "DMAD", NumDst: 1, NumSrc: 3, Output: OutputComp},
	OpDFRAC:           {Name: "DFRAC", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDLDEXP:          {Name: "DLDEXP", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpD2I:             {Name: "D2I", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpI2D:             {Name: "I2D", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpD2U:             {Name: "D2U", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpU2D:             {Name: "U2D", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDRSQ:            {Name: "DRSQ", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDTRUNC:          {Name: "DTRUNC", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDCEIL:           {Name: "DCEIL", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDFLR:            {Name: "DFLR", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDROUND:          {Name: "DROUND", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpDSSG:            {Name: "DSSG", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpVOTE_ANY:        {Name: "VOTE_ANY", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpVOTE_ALL:        {Name: "VOTE_ALL", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpVOTE_EQ:         {Name: "VOTE_EQ", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpU64SEQ:          {Name: "U64SEQ", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64SNE:          {Name: "U64SNE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64SLT:          {Name: "I64SLT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64SLT:          {Name: "U64SLT", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64SGE:          {Name: "I64SGE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64SGE:          {Name: "U64SGE", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64MIN:          {Name: "I64MIN", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64MIN:          {Name: "U64MIN", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64MAX:          {Name: "I64MAX", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64MAX:          {Name: "U64MAX", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64ABS:          {Name: "I64ABS", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpI64SSG:          {Name: "I64SSG", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpI64NEG:          {Name: "I64NEG", NumDst: 1, NumSrc: 1, Output: OutputComp},
	OpU64ADD:          {Name: "U64ADD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64MUL:          {Name: "U64MUL", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64SHL:          {Name: "U64SHL", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64SHR:          {Name: "I64SHR", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64SHR:          {Name: "U64SHR", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64DIV:          {Name: "I64DIV", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64DIV:          {Name: "U64DIV", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpI64MOD:          {Name: "I64MOD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpU64MOD:          {Name: "U64MOD", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpDDIV:            {Name: "DDIV", NumDst: 1, NumSrc: 2, Output: OutputComp},
	OpLOD:             {Name: "LOD", NumDst: 1, NumSrc: 3, Output: OutputOther},
	OpATOMINC_WRAP:    {Name: "ATOMINC_WRAP", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
	OpATOMDEC_WRAP:    {Name: "ATOMDEC_WRAP", NumDst: 1, NumSrc: 3, Output: OutputOther, IsStore: true},
}

// Info returns op's description, and whether op is an opcode.
func (op Opcode) Info() (*OpcodeInfo, bool) {
	if op >= NumOpcodes || opcodeInfos[op].Name == "" {
		return nil, false
	}
	return &opcodeInfos[op], true
}

func (op Opcode) String() string {
	if info, ok := op.Info(); ok {
		return info.Name
	}
	return "invalid opcode"
}

// opcodesByName are the opcodes by their names.
var opcodesByName = func() map[string]Opcode {
	m := make(map[string]Opcode, NumOpcodes)
	for op, info := range opcodeInfos {
		if info.Name != "" {
			m[info.Name] = Opcode(op)
		}
	}
	return m
}()
