    .section .text.start, "ax"
    .globl _start
_start:
    la sp, _stack_top
    j  main
