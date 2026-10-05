import { useCallback, useLayoutEffect, useSyncExternalStore, type SetStateAction } from "react";
import { draftOwner, migrateSessionDraft, observeSessionDraft, readSessionDraft, writeSessionDraft } from "./SessionDraft";

export function useSessionDraft(context: string, session: string) {
  const owner = draftOwner(context, session);
  useLayoutEffect(() => migrateSessionDraft(owner, session), [owner, session]);
  const subscribe = useCallback((notify: () => void) => observeSessionDraft(owner, notify), [owner]);
  const snapshot = useCallback(() => readSessionDraft(owner), [owner]);
  const text = useSyncExternalStore(subscribe, snapshot, () => "");
  const setText = useCallback((value: SetStateAction<string>) => {
    writeSessionDraft(owner, typeof value === "function" ? value(readSessionDraft(owner)) : value);
  }, [owner]);
  return [text, setText] as const;
}
