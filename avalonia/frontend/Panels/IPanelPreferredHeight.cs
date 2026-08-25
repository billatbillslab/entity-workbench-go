namespace EntityAvalonia.Panels;

// IPanelPreferredHeight lets a panel tell PanelStack how much vertical space it
// actually needs. Implement it only when the default slot height genuinely does
// not work — most panels are text or lists, they reflow, and they should take
// the stack default.
//
// Why this exists: the compute-program game panels (Snake, Life, Asteroids) draw
// a SQUARE world, so their drawing scales with min(width, height). In a stack of
// three slots the row is wide but short, which meant the square collapsed to the
// slot's leftover height (~135px in a 1280x1024 window) and threw away all the
// width. The board was legible only if the user hand-dragged a splitter. A panel
// that draws a fixed-aspect scene is the first content in this app whose size is
// a real requirement rather than a preference — so it needed a way to say so.
//
// Contract:
//   - The value is a MINIMUM, not a fixed height. The slot still star-shares the
//     viewport and the GridSplitter still moves freely above this floor.
//   - PanelStack clamps it to [SlotMinHeight, SlotMaxHeight]. It can never pin a
//     row to zero, so the layout-recursion SIGSEGV mitigation in PanelStack's
//     class doc stays intact.
//   - Exceeding the viewport is fine and intended: the stack's Grid.Height grows
//     to the summed minimums and the outer ScrollViewer engages. The user scrolls
//     rather than squinting.
public interface IPanelPreferredHeight
{
    // The minimum height, in DIPs, this panel needs to be usable.
    double PreferredSlotMinHeight { get; }
}
