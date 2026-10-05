import { useEffect, useState } from "react";
import {
  getCapabilities,
  onCapabilitiesChange,
  ensureCapabilities,
  type Capabilities,
} from "../capabilities";

/**
 * Subscribe to server capabilities (fetched once from /api/system/service).
 * Triggers the lazy fetch on first mount and re-renders on update.
 */
export function useCapabilities(): Capabilities {
  const [c, setC] = useState<Capabilities>(getCapabilities);
  useEffect(() => {
    void ensureCapabilities();
    return onCapabilitiesChange(setC);
  }, []);
  return c;
}
