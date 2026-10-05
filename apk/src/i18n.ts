// The RU dictionary and translator now live in @tgcontrol/shared (a union of the
// former Mini App + APK dictionaries). Re-exported here so the existing
// `../i18n` imports across the APK keep working unchanged. APK-only keys were
// merged into the shared dict; on keys present in both, the Mini App wording won.
export { t } from "@tgcontrol/shared";
