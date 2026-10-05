import type { CapacitorConfig } from "@capacitor/cli";

const config: CapacitorConfig = {
  appId: "com.tgcontrol.app",
  appName: "Remotai",
  webDir: "dist",
  server: {
    androidScheme: "https",
  },
  android: {
    allowMixedContent: true, // allow http servers on LAN
    // WebView debugging stays on for debug builds (Capacitor auto-enables it),
    // off for release — no forced override here.
  },
  plugins: {
    CapacitorHttp: { enabled: false }, // use browser fetch (we need custom headers)
  },
};

export default config;
