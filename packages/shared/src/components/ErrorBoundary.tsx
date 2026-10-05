import { Component, type ReactNode } from "react";
import { captureError } from "../observability";

interface Props {
  children: ReactNode;
  /** Render the fallback UI; `reset` clears the error state. */
  fallback: (reset: () => void) => ReactNode;
  /** When this value changes, the boundary clears a captured error (e.g. route change). */
  resetKey?: string;
}

interface State {
  hasError: boolean;
}

/**
 * Catches render/runtime errors in its subtree so a single crashing screen
 * doesn't white-screen the whole app. Reports to Sentry via captureError.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { hasError: false };

  static getDerivedStateFromError(): State {
    return { hasError: true };
  }

  componentDidCatch(error: unknown, info: unknown) {
    captureError(error, { boundary: "route", info });
  }

  componentDidUpdate(prev: Props) {
    if (this.state.hasError && prev.resetKey !== this.props.resetKey) {
      this.setState({ hasError: false });
    }
  }

  reset = () => this.setState({ hasError: false });

  render() {
    if (this.state.hasError) return this.props.fallback(this.reset);
    return this.props.children;
  }
}
