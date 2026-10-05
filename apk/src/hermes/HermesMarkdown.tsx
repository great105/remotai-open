import { memo } from "react";
import Markdown, { type Components, type UrlTransform } from "react-markdown";
import remarkGfm from "remark-gfm";
import { useStreamPresentation } from "./useStreamPresentation";

interface HermesMarkdownProps {
  children: string;
  className?: string;
  streaming?: boolean;
  /** Include chat/session identity when message IDs can be reused. */
  streamKey?: string;
}

// A relative destination needs a base only for protocol validation. Preserve
// the original URL so local anchors and Markdown references keep their meaning.
const safeUrl: UrlTransform = (url, key) => {
  if (key !== "href") return undefined;
  const value = url.trim();
  if (!value) return undefined;
  try {
    const protocol = new URL(value, "https://remotai.invalid/").protocol;
    return ["http:", "https:", "mailto:"].includes(protocol) ? value : undefined;
  } catch {
    return undefined;
  }
};

const components: Components = {
  a: ({ node: _node, href, children, ...props }) => {
    if (!href) return <span>{children}</span>;
    const anchor = href.startsWith("#");
    return <a {...props} href={href} target={anchor ? undefined : "_blank"}
      rel={anchor ? undefined : "noopener noreferrer"}>{children}</a>;
  },
  // Assistant Markdown is text. Keep image descriptions without silently
  // requesting remote resources or attempting to render local/private files.
  img: ({ alt }) => <span className="hermes-markdown-image-description">{alt}</span>,
  table: ({ node: _node, children, ...props }) => (
    <div className="hermes-markdown-table-scroll" tabIndex={0} role="region" aria-label="Таблица в ответе Hermes">
      <table {...props}>{children}</table>
    </div>
  ),
};

/** Renders a bounded live prefix; the page keeps the untouched source for copy. */
export const HermesMarkdown = memo(function HermesMarkdown({ children, className, streaming = false, streamKey }: HermesMarkdownProps) {
  const displayed = useStreamPresentation(children, streaming, streamKey);
  return (
    <div className={["hermes-markdown", className].filter(Boolean).join(" ")}>
      <Markdown remarkPlugins={[remarkGfm]} components={components} urlTransform={safeUrl} skipHtml>
        {displayed}
      </Markdown>
    </div>
  );
});
