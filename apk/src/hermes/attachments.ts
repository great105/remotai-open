import { t } from "@tgcontrol/shared";
import { HermesUserError, type HermesClient } from "./client";

export interface ChatAttachment {
  id: string;
  owner: string;
  file: File;
  progress: number;
  path?: string;
  error?: string;
  controller: AbortController;
}

/** Native file.attach stages host uploads for sandbox visibility and supplies
 * the reference syntax itself. Never guess @file quoting or inline binary data.
 * Reuse confirmed refs on retry so durable admission sees the same fingerprint.
 * A stale response may not stage the next file or submit a task in another chat.
 */
export async function stageAttachmentRefs(
  files: readonly ChatAttachment[], client: Pick<HermesClient, "rpc">,
  session: string, isCurrent: () => boolean, cache: Map<string, string>,
): Promise<string[] | null> {
  const refs: string[] = [];
  for (const item of files) {
    if (!isCurrent()) return null;
    // A resumed live ID is transient; staged files and an unresolved admission
    // survive backend restart. Re-staging would rename the copy and duplicate work.
    const key = item.id;
    let ref = cache.get(key);
    if (!ref) {
      const staged = await client.rpc<{attached: boolean; ref_text: string}>("file.attach", {
        profile: "default", session_id: session, path: item.path, name: item.file.name,
      });
      if (!isCurrent()) return null;
      if (!staged.attached || typeof staged.ref_text !== "string" || !staged.ref_text) {
        throw new HermesUserError(t("ui.attachments.m0d8fb0cbee"));
      }
      ref = staged.ref_text;
      cache.set(key, ref);
    }
    refs.push(ref);
  }
  return refs;
}
