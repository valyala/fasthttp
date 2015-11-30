TEXT main.AppendUint1(SB) /home/eric/gopath/src/github.com/EricLagergren/fasthttp/_testdata/appenduint.go
	appenduint.go:18	0x401000	64488b0c25f8ffffff	FS MOVQ FS:0xfffffff8, CX
	appenduint.go:18	0x401009	483b6110		CMPQ 0x10(CX), SP
	appenduint.go:18	0x40100d	0f866b010000		JBE 0x40117e
	appenduint.go:18	0x401013	4883ec40		SUBQ $0x40, SP
	appenduint.go:18	0x401017	31db			XORL BX, BX
	appenduint.go:18	0x401019	48895c2450		MOVQ BX, 0x50(SP)
	appenduint.go:18	0x40101e	48895c2458		MOVQ BX, 0x58(SP)
	appenduint.go:18	0x401023	48895c2460		MOVQ BX, 0x60(SP)
	appenduint.go:19	0x401028	488b5c2448		MOVQ 0x48(SP), BX
	appenduint.go:19	0x40102d	4883fb00		CMPQ $0x0, BX
	appenduint.go:19	0x401031	7d54			JGE 0x401087
	appenduint.go:20	0x401033	488d1d86bf0700		LEAQ 0x7bf86(IP), BX
	appenduint.go:20	0x40103a	48895c2430		MOVQ BX, 0x30(SP)
	appenduint.go:20	0x40103f	48c744243819000000	MOVQ $0x19, 0x38(SP)
	appenduint.go:20	0x401048	488d1d91710500		LEAQ 0x57191(IP), BX
	appenduint.go:20	0x40104f	48891c24		MOVQ BX, 0(SP)
	appenduint.go:20	0x401053	488d5c2430		LEAQ 0x30(SP), BX
	appenduint.go:20	0x401058	48895c2408		MOVQ BX, 0x8(SP)
	appenduint.go:20	0x40105d	48c744241000000000	MOVQ $0x0, 0x10(SP)
	appenduint.go:20	0x401066	e815640000		CALL runtime.convT2E(SB)
	appenduint.go:20	0x40106b	488d5c2418		LEAQ 0x18(SP), BX
	appenduint.go:20	0x401070	488b0b			MOVQ 0(BX), CX
	appenduint.go:20	0x401073	48890c24		MOVQ CX, 0(SP)
	appenduint.go:20	0x401077	488b4b08		MOVQ 0x8(BX), CX
	appenduint.go:20	0x40107b	48894c2408		MOVQ CX, 0x8(SP)
	appenduint.go:20	0x401080	e8bbfe0100		CALL runtime.gopanic(SB)
	appenduint.go:20	0x401085	0f0b			UD2

	// Body
	appenduint.go:23	0x401087	488b0532460d00		MOVQ 0xd4632(IP), AX
	appenduint.go:23	0x40108e	4883c008		ADDQ $0x8, AX
	appenduint.go:23	0x401092	488d1d27460500		LEAQ 0x54627(IP), BX
	appenduint.go:23	0x401099	48891c24		MOVQ BX, 0(SP)
	appenduint.go:23	0x40109d	4889442408		MOVQ AX, 0x8(SP)
	appenduint.go:23	0x4010a2	4889442410		MOVQ AX, 0x10(SP)
	appenduint.go:23	0x4010a7	e8f41f0300		CALL runtime.makeslice(SB)
	appenduint.go:23	0x4010ac	488b4c2448		MOVQ 0x48(SP), CX
	appenduint.go:23	0x4010b1	4c8b542418		MOVQ 0x18(SP), R10
	appenduint.go:23	0x4010b6	488b742420		MOVQ 0x20(SP), SI
	appenduint.go:23	0x4010bb	4c8b5c2428		MOVQ 0x28(SP), R11
	appenduint.go:24	0x4010c0	4889f7			MOVQ SI, DI
	appenduint.go:24	0x4010c3	48ffce			DECQ SI
	appenduint.go:26	0x4010c6	4839fe			CMPQ DI, SI
	appenduint.go:26	0x4010c9	0f83a8000000		JAE 0x401177
	appenduint.go:26	0x4010cf	498d1c32		LEAQ 0(R10)(SI*1), BX
	appenduint.go:26	0x4010d3	4889cd			MOVQ CX, BP
	appenduint.go:26	0x4010d6	49b96766666666666666	MOVQ $0x6666666666666667, R9
	appenduint.go:26	0x4010e0	4889c8			MOVQ CX, AX
	appenduint.go:26	0x4010e3	49f7e9			IMULQ R9
	appenduint.go:26	0x4010e6	4989d0			MOVQ DX, R8
	appenduint.go:26	0x4010e9	49c1f802		SARQ $0x2, R8
	appenduint.go:26	0x4010ed	48c1fd3f		SARQ $0x3f, BP
	appenduint.go:26	0x4010f1	4929e8			SUBQ BP, R8
	appenduint.go:26	0x4010f4	4c89c5			MOVQ R8, BP
	appenduint.go:26	0x4010f7	486bed0a		IMULQ $0xa, BP, BP
	appenduint.go:26	0x4010fb	4989c8			MOVQ CX, R8
	appenduint.go:26	0x4010fe	4929e8			SUBQ BP, R8
	appenduint.go:26	0x401101	4c89c5			MOVQ R8, BP
	appenduint.go:26	0x401104	4883c530		ADDQ $0x30, BP
	appenduint.go:26	0x401108	40882b			MOVL BP, 0(BX)
	appenduint.go:27	0x40110b	4889cd			MOVQ CX, BP
	appenduint.go:27	0x40110e	49b96766666666666666	MOVQ $0x6666666666666667, R9
	appenduint.go:27	0x401118	4889c8			MOVQ CX, AX
	appenduint.go:27	0x40111b	49f7e9			IMULQ R9
	appenduint.go:27	0x40111e	4889d1			MOVQ DX, CX
	appenduint.go:27	0x401121	48c1f902		SARQ $0x2, CX
	appenduint.go:27	0x401125	48c1fd3f		SARQ $0x3f, BP
	appenduint.go:27	0x401129	4829e9			SUBQ BP, CX
	appenduint.go:28	0x40112c	4883f900		CMPQ $0x0, CX
	appenduint.go:28	0x401130	7539			JNE 0x40116b
	appenduint.go:34	0x401132	4889fd			MOVQ DI, BP
	appenduint.go:34	0x401135	4d89d8			MOVQ R11, R8
	appenduint.go:34	0x401138	4839fe			CMPQ DI, SI
	appenduint.go:34	0x40113b	7727			JA 0x401164
	appenduint.go:34	0x40113d	4829f5			SUBQ SI, BP
	appenduint.go:34	0x401140	4929f0			SUBQ SI, R8
	appenduint.go:34	0x401143	4d89d1			MOVQ R10, R9
	appenduint.go:34	0x401146	4983f800		CMPQ $0x0, R8
	appenduint.go:34	0x40114a	7404			JE 0x401150
	appenduint.go:34	0x40114c	4d8d0c31		LEAQ 0(R9)(SI*1), R9
	appenduint.go:34	0x401150	4c894c2450		MOVQ R9, 0x50(SP)
	appenduint.go:34	0x401155	48896c2458		MOVQ BP, 0x58(SP)
	appenduint.go:34	0x40115a	4c89442460		MOVQ R8, 0x60(SP)
	appenduint.go:34	0x40115f	4883c440		ADDQ $0x40, SP
	appenduint.go:34	0x401163	c3			RET
	appenduint.go:34	0x401164	e8b7e70100		CALL runtime.panicslice(SB)
	appenduint.go:34	0x401169	0f0b			UD2
	appenduint.go:31	0x40116b	48ffce			DECQ SI
	appenduint.go:26	0x40116e	4839fe			CMPQ DI, SI
	appenduint.go:26	0x401171	0f8258ffffff		JB 0x4010cf
	appenduint.go:26	0x401177	e844e70100		CALL runtime.panicindex(SB)
	appenduint.go:26	0x40117c	0f0b			UD2
	appenduint.go:18	0x40117e	e86d9d0400		CALL runtime.morestack_noctxt(SB)
	appenduint.go:18	0x401183	e978feffff		JMP main.AppendUint1(SB)
	appenduint.go:18	0x401188	0000			ADDL AL, 0(AX)
	appenduint.go:18	0x40118a	0000			ADDL AL, 0(AX)
	appenduint.go:18	0x40118c	0000			ADDL AL, 0(AX)
	appenduint.go:18	0x40118e	0000			ADDL AL, 0(AX)