#include "textflag.h"

// NEON kernels for float32 squared L2 distance and inner product.
//
// Each iteration loads 16 floats from each input (four 128-bit registers) and
// updates four independent accumulators, so the fused multiply-adds of one
// iteration do not wait on each other. n must be a multiple of 16; the caller
// handles the tail. Vector FADD, FSUB and FADDP are not mnemonics in the Go
// assembler, so they are emitted as instruction words:
//   FADD  Vd.4S, Vn.4S, Vm.4S = 0x4E20D400 | Rm<<16 | Rn<<5 | Rd
//   FSUB  Vd.4S, Vn.4S, Vm.4S = 0x4EA0D400 | Rm<<16 | Rn<<5 | Rd
//   FADDP Vd.4S, Vn.4S, Vm.4S = 0x6E20D400 | Rm<<16 | Rn<<5 | Rd

// func l2sqNEON(a, b *float32, n int) float32
TEXT ·l2sqNEON(SB), NOSPLIT, $0-28
	MOVD a+0(FP), R0
	MOVD b+8(FP), R1
	MOVD n+16(FP), R2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16

l2loop:
	CBZ    R2, l2done
	VLD1.P 64(R0), [V4.S4, V5.S4, V6.S4, V7.S4]
	VLD1.P 64(R1), [V16.S4, V17.S4, V18.S4, V19.S4]
	WORD   $0x4EB0D484 // FSUB V4.4S, V4.4S, V16.4S
	WORD   $0x4EB1D4A5 // FSUB V5.4S, V5.4S, V17.4S
	WORD   $0x4EB2D4C6 // FSUB V6.4S, V6.4S, V18.4S
	WORD   $0x4EB3D4E7 // FSUB V7.4S, V7.4S, V19.4S
	VFMLA  V4.S4, V4.S4, V0.S4
	VFMLA  V5.S4, V5.S4, V1.S4
	VFMLA  V6.S4, V6.S4, V2.S4
	VFMLA  V7.S4, V7.S4, V3.S4
	SUB    $16, R2
	B      l2loop

l2done:
	WORD  $0x4E21D400 // FADD  V0.4S, V0.4S, V1.4S
	WORD  $0x4E23D442 // FADD  V2.4S, V2.4S, V3.4S
	WORD  $0x4E22D400 // FADD  V0.4S, V0.4S, V2.4S
	WORD  $0x6E20D400 // FADDP V0.4S, V0.4S, V0.4S
	WORD  $0x6E20D400 // FADDP V0.4S, V0.4S, V0.4S
	FMOVS F0, ret+24(FP)
	RET

// func dotNEON(a, b *float32, n int) float32
TEXT ·dotNEON(SB), NOSPLIT, $0-28
	MOVD a+0(FP), R0
	MOVD b+8(FP), R1
	MOVD n+16(FP), R2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16

dotloop:
	CBZ    R2, dotdone
	VLD1.P 64(R0), [V4.S4, V5.S4, V6.S4, V7.S4]
	VLD1.P 64(R1), [V16.S4, V17.S4, V18.S4, V19.S4]
	VFMLA  V4.S4, V16.S4, V0.S4
	VFMLA  V5.S4, V17.S4, V1.S4
	VFMLA  V6.S4, V18.S4, V2.S4
	VFMLA  V7.S4, V19.S4, V3.S4
	SUB    $16, R2
	B      dotloop

dotdone:
	WORD  $0x4E21D400 // FADD  V0.4S, V0.4S, V1.4S
	WORD  $0x4E23D442 // FADD  V2.4S, V2.4S, V3.4S
	WORD  $0x4E22D400 // FADD  V0.4S, V0.4S, V2.4S
	WORD  $0x6E20D400 // FADDP V0.4S, V0.4S, V0.4S
	WORD  $0x6E20D400 // FADDP V0.4S, V0.4S, V0.4S
	FMOVS F0, ret+24(FP)
	RET
