# Silicon Labs WSTK (native): hardware results

Device identifiers below are fictional (`<serial>`); board numbers and
revisions are model facts.

Kit: BRD4001A Rev A02 mainboard with a BRD4183A Rev A00 radio board
(EFR32MG22C224F512IM40), USB to macOS, kit firmware 1.4.6 build 1171. The
target ran unrelated firmware owned by another user; hwlab did not program,
reset, halt or power-cycle it. The J-Link was shared with that user, so runs
were short and started only when no other J-Link client was active.

## USB (EMUCOM)

| Check | Result |
|---|---|
| Discovery | `wstk/wstk:<serial>`, named "Silicon Labs Wireless Starter Kit (BRD4001A + BRD4183A)", with board serials/revisions/dates, firmware, VCOM path and links to `jlink`, `jlinknative` and `uart` devices |
| `PowerMeasurement.info` | firmware 1.4.6 b1171, both board records |
| `read_current` | 256 samples, mean 2.18 mA, 3.336 V |
| AEM 1 s | 10,000 samples |
| AEM 5 s (×2, hardware test) | 50,000 samples each, no gaps or step jumps; one run's samples spanned 5.025 s of session time (cause not established, probably a clock-offset step at start) |
| `hwlab run` 5 s + `hwlab analyze` | 50,000 samples; mean 2.182 mA, min 2.095 mA, max 2.479 mA; charge 10,911 µC, energy 36,403 µJ; plot rendered |
| Clock | jitter of the per-buffer mapping 1.1–5.6 ms; kit vs host drift about −50 ppm (few-second runs, rough) |

The first capture attempt stalled after the first full-packet read: the
trailing zero-length packet was not consumed. Fixed in the EMUCOM reader;
all later runs were clean.

## Not run on hardware

* Ethernet (admin 4902, DCH 4905, AEM polling, PTI → pcapng): no kit on the
  LAN; synthetic tests only.
* PTI: no Ethernet, and the target firmware was not known to enable PTI.
* AEM accuracy against a reference load.

## Incident

While mapping the protocol with the vendor tools (before the plugin existed),
an admin-console `target status` sent through `silink` made the kit's board
controller re-enumerate on USB. It recovered by itself; the effect on the
other user's debug session is unknown. hwlab's admin allowlist excludes all
`target` commands.
