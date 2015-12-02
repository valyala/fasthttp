TEXT main.AppendUint2(SB) /home/eric/gopath/src/github.com/EricLagergren/fasthttp/_testdata/appenduint.go
	appenduint.go:37	0x401190	64488b0c25f8ffffff	FS MOVQ FS:0xfffffff8, CX
	appenduint.go:37	0x401199	483b6110		CMPQ 0x10(CX), SP
	appenduint.go:37	0x40119d	0f8652010000		JBE 0x4012f5
	appenduint.go:37	0x4011a3	4883ec40		SUBQ $0x40, SP
	appenduint.go:37	0x4011a7	488b442448		MOVQ 0x48(SP), AX
	appenduint.go:37	0x4011ac	31db			XORL BX, BX
	appenduint.go:37	0x4011ae	48895c2450		MOVQ BX, 0x50(SP)
	appenduint.go:37	0x4011b3	48895c2458		MOVQ BX, 0x58(SP)
	appenduint.go:37	0x4011b8	48895c2460		MOVQ BX, 0x60(SP)
	appenduint.go:38	0x4011bd	4883f800		CMPQ $0x0, AX
	appenduint.go:38	0x4011c1	7d54			JGE 0x401217
	appenduint.go:39	0x4011c3	488d1d1ee30600		LEAQ 0x6e31e(IP), BX
	appenduint.go:39	0x4011ca	48895c2430		MOVQ BX, 0x30(SP)
	appenduint.go:39	0x4011cf	48c744243806000000	MOVQ $0x6, 0x38(SP)
	appenduint.go:39	0x4011d8	488d1d01700500		LEAQ 0x57001(IP), BX
	appenduint.go:39	0x4011df	48891c24		MOVQ BX, 0(SP)
	appenduint.go:39	0x4011e3	488d5c2430		LEAQ 0x30(SP), BX
	appenduint.go:39	0x4011e8	48895c2408		MOVQ BX, 0x8(SP)
	appenduint.go:39	0x4011ed	48c744241000000000	MOVQ $0x0, 0x10(SP)
	appenduint.go:39	0x4011f6	e885620000		CALL runtime.convT2E(SB)
	appenduint.go:39	0x4011fb	488d5c2418		LEAQ 0x18(SP), BX
	appenduint.go:39	0x401200	488b0b			MOVQ 0(BX), CX
	appenduint.go:39	0x401203	48890c24		MOVQ CX, 0(SP)
	appenduint.go:39	0x401207	488b4b08		MOVQ 0x8(BX), CX
	appenduint.go:39	0x40120b	48894c2408		MOVQ CX, 0x8(SP)
	appenduint.go:39	0x401210	e82bfd0100		CALL runtime.gopanic(SB)
	appenduint.go:39	0x401215	0f0b			UD2

	// Body
	appenduint.go:42	0x401217	4889442428		MOVQ AX, 0x28(SP)
	appenduint.go:44	0x40121c	488d1d1d610500		LEAQ 0x5611d(IP), BX
	appenduint.go:44	0x401223	48891c24		MOVQ BX, 0(SP)
	appenduint.go:44	0x401227	e8548c0000		CALL runtime.newobject(SB)
	appenduint.go:44	0x40122c	488b7c2408		MOVQ 0x8(SP), DI
	appenduint.go:62	0x401231	488b4c2428		MOVQ 0x28(SP), CX
	appenduint.go:45	0x401236	48c7c640000000		MOVQ $0x40, SI
	appenduint.go:63	0x40123d	4883f90a		CMPQ $0xa, CX
	appenduint.go:63	0x401241	7248			JB 0x40128b
	appenduint.go:64	0x401243	48ffce			DECQ SI
	appenduint.go:65	0x401246	49b9cdcccccccccccccc	MOVQ $0xcccccccccccccccd, R9
	appenduint.go:65	0x401250	4889c8			MOVQ CX, AX
	appenduint.go:65	0x401253	49f7e1			MULQ R9
	appenduint.go:65	0x401256	4889d0			MOVQ DX, AX
	appenduint.go:65	0x401259	48c1e803		SHRQ $0x3, AX
	appenduint.go:66	0x40125d	4883fe40		CMPQ $0x40, SI
	appenduint.go:66	0x401261	0f8387000000		JAE 0x4012ee
	appenduint.go:66	0x401267	488d1c37		LEAQ 0(DI)(SI*1), BX
	appenduint.go:66	0x40126b	4889c5			MOVQ AX, BP
	appenduint.go:66	0x40126e	486bed0a		IMULQ $0xa, BP, BP
	appenduint.go:66	0x401272	4989c8			MOVQ CX, R8
	appenduint.go:66	0x401275	4929e8			SUBQ BP, R8
	appenduint.go:66	0x401278	4c89c5			MOVQ R8, BP
	appenduint.go:66	0x40127b	4883c530		ADDQ $0x30, BP
	appenduint.go:66	0x40127f	40882b			MOVL BP, 0(BX)
	appenduint.go:67	0x401282	4889c1			MOVQ AX, CX
	appenduint.go:63	0x401285	4883f90a		CMPQ $0xa, CX
	appenduint.go:63	0x401289	73b8			JAE 0x401243
	appenduint.go:70	0x40128b	4889f0			MOVQ SI, AX
	appenduint.go:70	0x40128e	48ffc8			DECQ AX
	appenduint.go:71	0x401291	4883f840		CMPQ $0x40, AX
	appenduint.go:71	0x401295	7350			JAE 0x4012e7
	appenduint.go:71	0x401297	488d1c07		LEAQ 0(DI)(AX*1), BX
	appenduint.go:71	0x40129b	4889cd			MOVQ CX, BP
	appenduint.go:71	0x40129e	4883c530		ADDQ $0x30, BP
	appenduint.go:71	0x4012a2	40882b			MOVL BP, 0(BX)
	appenduint.go:73	0x4012a5	4883f840		CMPQ $0x40, AX
	appenduint.go:73	0x4012a9	7735			JA 0x4012e0
	appenduint.go:73	0x4012ab	48c7c540000000		MOVQ $0x40, BP
	appenduint.go:73	0x4012b2	4829c5			SUBQ AX, BP
	appenduint.go:73	0x4012b5	4989f8			MOVQ DI, R8
	appenduint.go:73	0x4012b8	4883ff00		CMPQ $0x0, DI
	appenduint.go:73	0x4012bc	741e			JE 0x4012dc
	appenduint.go:73	0x4012be	4883fd00		CMPQ $0x0, BP
	appenduint.go:73	0x4012c2	7404			JE 0x4012c8
	appenduint.go:73	0x4012c4	4d8d0400		LEAQ 0(R8)(AX*1), R8
	appenduint.go:73	0x4012c8	4c89442450		MOVQ R8, 0x50(SP)
	appenduint.go:73	0x4012cd	48896c2458		MOVQ BP, 0x58(SP)
	appenduint.go:73	0x4012d2	48896c2460		MOVQ BP, 0x60(SP)
	appenduint.go:73	0x4012d7	4883c440		ADDQ $0x40, SP
	appenduint.go:73	0x4012db	c3			RET
	appenduint.go:73	0x4012dc	8907			MOVL AX, 0(DI)
	appenduint.go:73	0x4012de	ebde			JMP 0x4012be
	appenduint.go:73	0x4012e0	e83be60100		CALL runtime.panicslice(SB)
	appenduint.go:73	0x4012e5	0f0b			UD2
	appenduint.go:71	0x4012e7	e8d4e50100		CALL runtime.panicindex(SB)
	appenduint.go:71	0x4012ec	0f0b			UD2
	appenduint.go:66	0x4012ee	e8cde50100		CALL runtime.panicindex(SB)
	appenduint.go:66	0x4012f3	0f0b			UD2
	appenduint.go:37	0x4012f5	e8f69b0400		CALL runtime.morestack_noctxt(SB)
	appenduint.go:37	0x4012fa	e991feffff		JMP main.AppendUint2(SB)
	appenduint.go:37	0x4012ff	00			?
