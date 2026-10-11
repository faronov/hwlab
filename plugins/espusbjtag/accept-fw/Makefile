# Builds the ESP32-C6 acceptance image. Needs a riscv64-unknown-elf GCC that
# can target rv32imac; nothing here runs on the host at test time.
RVCC ?= riscv64-unknown-elf-gcc
CFLAGS = -march=rv32imac_zicsr -mabi=ilp32 -Os -nostdlib -ffreestanding -fno-pic -Wall
OUT ?= build

all: $(OUT)/accept.bin

$(OUT)/accept.elf: start.S main.c link.ld
	mkdir -p $(OUT)
	$(RVCC) $(CFLAGS) -T link.ld -Wl,--gc-sections -o $@ start.S main.c

$(OUT)/accept.flat: $(OUT)/accept.elf
	riscv64-unknown-elf-objcopy -O binary $< $@

$(OUT)/accept.bin: $(OUT)/accept.flat
	cd ../../.. && go run ./plugins/espusbjtag/accept-fw/mkimage plugins/espusbjtag/accept-fw/$< 0x40850000 0x40850000 plugins/espusbjtag/accept-fw/$@
	riscv64-unknown-elf-nm $(OUT)/accept.elf | grep -E 'hwlab_|_start|main' > $(OUT)/accept.sym
	sh ../../../scripts/build-ownership.sh record "$(OUT)" accept.elf accept.flat accept.bin accept.sym

clean:
	sh ../../../scripts/build-ownership.sh clean "$(OUT)"
