/* Acceptance image: one flat RAM segment loaded by the ROM into HP SRAM. */
ENTRY(_start)
MEMORY { SRAM (rwx) : ORIGIN = 0x40850000, LENGTH = 0x4000 }
SECTIONS
{
  .text : { *(.text.start) *(.text*) *(.rodata*) } > SRAM
  .data : { *(.data*) } > SRAM
  _stack_top = ORIGIN(SRAM) + LENGTH(SRAM);
  /DISCARD/ : { *(.comment) *(.riscv.attributes) }
}
