/* link.ld -- deterministic layout for the flat trampoline blob.
 *
 * Linked at address 0 by design: every reference the compiler emits to
 * g_state must be RIP-relative (build.sh checks the object's relocations), so
 * the address the blob is eventually mapped at never appears in the bytes. The
 * three hook stubs come first, in a fixed order, followed by the state struct,
 * which must be the last thing in the blob so that
 * StateOffset == len(Payload) - StateSize.
 */
ENTRY(clock_gettime_entry)

SECTIONS
{
    . = 0;

    .text : {
        *(.tramp_text.clock_gettime)
        *(.tramp_text.gettimeofday)
        *(.tramp_text.time)
    }

    . = ALIGN(8);
    .data : {
        *(.tramp_state)
    }

    /DISCARD/ : {
        *(.comment)
        *(.eh_frame)
        *(.note.*)
    }
}
