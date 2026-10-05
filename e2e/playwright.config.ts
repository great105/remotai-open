import { defineConfig, devices } from "@playwright/test";

const PORT = process.env.TGCONTROL_PORT || "8080";
const BASE_URL = process.env.TGCONTROL_URL || `http://localhost:${PORT}`;

export default defineConfig({
  testDir: "./tests",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: 1,
  reporter: process.env.CI ? "github" : "list",
  timeout: 60_000,
  expect: { timeout: 10_000 },

  use: {
    baseURL: BASE_URL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
    // Tests authenticate via API token → JWT in localStorage; the auth fixture
    // attaches the bearer token to all API requests.
  },

  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],

  // Tests assume tgcontrol.exe is already running on PORT. We do NOT auto-start
  // it because it conflicts with the Windows Service. Start it manually first:
  //   ./tgcontrol.exe   (or use the installed service)
});
