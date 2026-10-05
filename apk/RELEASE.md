# TGControl APK — production build & release

Standalone Android client (Capacitor 8 + React/Vite). This document covers the
reproducible **signed** release build for Google Play.

## Prerequisites

- **Node.js** ≥ 20 (`npm install` in `apk/`)
- **JDK 21** — Capacitor 8 / AGP compile at Java 21 (`capacitor.build.gradle`
  pins `VERSION_21`). JDK 17 will **not** build the release.
  Installed here: `C:\Program Files\Java\jdk-21.0.7+6`.
- **Android SDK** — path in `android/local.properties` (`sdk.dir`).

## One-time: release signing

Release builds are signed with a keystore that is **never** committed
(`android/.gitignore` excludes `*.keystore` and `keystore.properties`).

1. The keystore lives at `android/tgcontrol-release.keystore`
   (alias `tgcontrol`, valid until 2053).
2. Credentials are read from `android/keystore.properties` — see
   `keystore.properties.example` for the format. `app/build.gradle` loads this
   file into `signingConfigs.release`; if it is missing the release build fails
   loudly (debug builds are unaffected).

> ⚠️ **Back up `tgcontrol-release.keystore` and its passwords offline.** Losing
> the key means you can never publish an update to the same Play listing.
> Treat it like a production secret.

To create a fresh keystore (only if starting a new listing):

```bash
keytool -genkeypair -v -keystore android/tgcontrol-release.keystore \
  -alias tgcontrol -keyalg RSA -keysize 2048 -validity 10000 -storetype PKCS12
```

## Build

```bash
# 1. Web bundle (tsc + vite, production drops console.* / debugger)
npm run build

# 2. Copy web assets + plugins into the native project
npx cap sync android

# 3. Signed release artifacts (run with JDK 21 on PATH / JAVA_HOME)
cd android
./gradlew.bat bundleRelease     # -> app/build/outputs/bundle/release/app-release.aab  (Play Store)
./gradlew.bat assembleRelease   # -> app/build/outputs/apk/release/app-release.apk     (sideload/testing)
```

PowerShell one-liner forcing JDK 21:

```powershell
$env:JAVA_HOME = "C:\Program Files\Java\jdk-21.0.7+6"
cd apk\android; .\gradlew.bat bundleRelease assembleRelease
```

### Verify signature

```bash
# AAB (signed as a jar)
jarsigner -verify -verbose app/build/outputs/bundle/release/app-release.aab

# APK (preferred)
"$ANDROID_SDK/build-tools/<ver>/apksigner" verify --print-certs \
  app/build/outputs/apk/release/app-release.apk
```

## Versioning

Bump both fields in `android/app/build.gradle` for every Play upload:

- `versionCode` — integer, must strictly increase on each upload.
- `versionName` — human-readable semver, kept in sync with `package.json`.

Current: `versionCode 14`, `versionName "2.9.3"`.

## Release hardening already in place

- **R8 minify + resource shrink** with Capacitor/WebView keep rules
  (`app/proguard-rules.pro`).
- **Network security config** (`res/xml/network_security_config.xml`): cleartext
  allowed for LAN self-hosting, TLS enforced for the cloud relay; public-host
  HTTP is rejected at the app layer (`src/config.ts`).
- **Sentry** crash reporting (opt-in) with source/line attributes preserved.
- No native `alert/confirm/prompt` — all dialogs go through the in-app
  `DialogHost`. All UI strings are Russian via `src/i18n.ts`.
