import { getLanguage } from "./locale";
import { ownedEnglish } from "./owned-messages.en";
import { messageForSource } from "./i18n";

/** Translate only Remotai's built-in metadata, never arbitrary response content. */
export function ownedText(text: string): string {
  if (getLanguage() !== "en") return text;
  if (Object.prototype.hasOwnProperty.call(ownedEnglish, text)) return ownedEnglish[text];
  // The VPN catalog appends this fixed explanation to a runtime list of names.
  for (const suffix of Object.keys(ownedEnglish)) {
    if (suffix.startsWith(". ") && text.endsWith(suffix)) return text.slice(0, -suffix.length) + ownedEnglish[suffix];
  }
  return messageForSource(text);
}
