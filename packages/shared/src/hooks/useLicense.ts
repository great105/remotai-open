import { useState, useEffect } from "react";
import { getLicenseStatus } from "../api-endpoints";
import type { LicenseStatus } from "../types";

let cachedLicense: LicenseStatus | null = null;
let fetchPromise: Promise<LicenseStatus> | null = null;
const listeners = new Set<(l: LicenseStatus) => void>();

function notifyListeners(l: LicenseStatus) {
  cachedLicense = l;
  listeners.forEach((fn) => fn(l));
}

export function refreshLicense() {
  fetchPromise = getLicenseStatus().then((l) => {
    notifyListeners(l);
    return l;
  });
  return fetchPromise;
}

export function useLicense(enabled = true) {
  const [license, setLicense] = useState<LicenseStatus | null>(cachedLicense);

  useEffect(() => {
    listeners.add(setLicense);
    if (!enabled) return () => { listeners.delete(setLicense); };
    if (!cachedLicense && !fetchPromise) {
      refreshLicense().catch(() => {});
    } else if (cachedLicense) {
      setLicense(cachedLicense);
    }
    return () => { listeners.delete(setLicense); };
  }, [enabled]);

  const canUse = (feature: keyof LicenseStatus["limits"]): boolean => {
    if (!license) return true; // Allow until license loads
    const val = license.limits[feature];
    if (typeof val === "boolean") return val;
    return val !== 0; // 0 means unlimited for numeric limits
  };

  return {
    license,
    tier: license?.tier ?? "free",
    limits: license?.limits ?? null,
    canUse,
    refresh: refreshLicense,
  };
}
