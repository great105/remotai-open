import { useEffect, useState } from "react";
import { getFeatures, onFeaturesChange, type Features } from "../features";

/** Subscribe to feature flag changes and re-render on update. */
export function useFeatures(): Features {
  const [f, setF] = useState<Features>(getFeatures);
  useEffect(() => onFeaturesChange(setF), []);
  return f;
}
