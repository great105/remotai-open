import type { PtyStatusIcon } from "../ptyTerm/listStatus";
import { IconCheck, IconHourglass, IconStopwatch, IconUnlink, IconWarning } from "./icons";

export function ptyStatusIcon(icon: PtyStatusIcon) {
  switch (icon) {
    case "stalled": return <IconStopwatch size={11} />;
    case "waiting": return <IconHourglass size={11} />;
    case "ready": return <IconCheck size={11} />;
    case "error": return <IconWarning size={11} />;
    case "link": return <IconUnlink size={11} />;
    case "working": return "●";
    case "sleep": return "💤";
    default: return null;
  }
}
