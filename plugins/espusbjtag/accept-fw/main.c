/*
 * hwlab acceptance image for the ESP32-C6 (HP core). Not a product firmware.
 *
 * It replaces the second-stage bootloader slot only for the length of the
 * acceptance test: it stops the watchdogs, then increments a counter in SRAM
 * in a tight loop and prints a banner on the USB Serial/JTAG CDC port.
 *
 * Register addresses are from the ESP32-C6 technical reference manual.
 */
typedef unsigned int u32;
#define REG(a) (*(volatile u32 *)(a))

#define WKEY        0x50D83AA1u
#define TIMG0       0x60008000u
#define TIMG1       0x60009000u
#define LP_WDT      0x600B1C00u
#define USB_EP1     0x6000F000u
#define USB_EP1CONF 0x6000F004u

/* Fixed, nm-visible symbols the host tests use. */
volatile u32 hwlab_counter __attribute__((section(".data"), used)) = 1;
volatile u32 hwlab_scratch[16] __attribute__((section(".data"), used)) = {0};

static void wdt_off(void)
{
    REG(TIMG0 + 0x64) = WKEY;
    REG(TIMG0 + 0x48) = 0;
    REG(TIMG0 + 0x64) = 0;
    REG(TIMG1 + 0x64) = WKEY;
    REG(TIMG1 + 0x48) = 0;
    REG(TIMG1 + 0x64) = 0;
    REG(LP_WDT + 0x18) = WKEY;
    REG(LP_WDT + 0x00) = 0;
    REG(LP_WDT + 0x18) = 0;
    REG(LP_WDT + 0x20) = WKEY;
    REG(LP_WDT + 0x1C) = 1u << 30; /* super watchdog off */
    REG(LP_WDT + 0x20) = 0;
}

static void put(char c)
{
    if (REG(USB_EP1CONF) & 2u) {
        REG(USB_EP1) = (u32)(unsigned char)c;
    }
}

static void flush(void)
{
    REG(USB_EP1CONF) = 1u; /* WR_DONE */
}

static void banner(u32 n)
{
    static const char hex[] = "0123456789abcdef";
    const char *s = "HWLAB-ESP32C6-ACCEPT-1 counter=";
    while (*s) put(*s++);
    for (int i = 28; i >= 0; i -= 4) put(hex[(n >> i) & 15]);
    put('\r');
    put('\n');
    flush();
}

/* Kept as a real call so that a breakpoint has a stable, hot address. */
__attribute__((noinline)) void hwlab_tick(void)
{
    hwlab_counter = hwlab_counter + 1;
}

void main(void)
{
    wdt_off();
    banner(0);
    for (;;) {
        hwlab_tick();
        if ((hwlab_counter & 0x3FFFF) == 0) banner(hwlab_counter);
    }
}
