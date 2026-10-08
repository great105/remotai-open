export type FolderTab = "fav" | "recent" | "projects" | "browse";
export type FolderSort = "name-asc" | "name-desc" | "date-desc";

export interface FolderEntry {
  name: string;
  path: string;
  modified?: number | null;
  time?: number;
}

export interface FolderListOptions {
  query: string;
  sort: FolderSort;
  locale: string;
  hideDotFolders?: boolean;
  favorites?: readonly { path: string }[];
  searchPath?: boolean;
  label?: (item: FolderEntry) => string;
}

/** Windows paths are insensitive to case and separator; Unix paths are not. */
export function folderPathKey(path: string): string {
  const windows = /^[a-z]:/i.test(path) || /^[/\\]{2}/.test(path);
  const normalized = (windows ? path.replace(/\\/g, "/") : path).replace(/\/+$/, "") || "/";
  return windows ? normalized.toLowerCase() : normalized;
}

export function selectFolders<T extends FolderEntry>(items: readonly T[], options: FolderListOptions): T[] {
  const query = options.query.trim().toLocaleLowerCase(options.locale);
  const favorites = options.favorites ? new Set(options.favorites.map(item => folderPathKey(item.path))) : null;
  const label = (item: T) => options.label?.(item) || item.name;
  const collator = new Intl.Collator(options.locale, { numeric: true, sensitivity: "base" });
  const byName = (a: T, b: T) => collator.compare(label(a), label(b)) || collator.compare(a.path, b.path);
  const timestamp = (item: T) => item.time ?? item.modified;
  return items.filter(item => {
    if (options.hideDotFolders && item.name.startsWith(".")) return false;
    if (favorites && !favorites.has(folderPathKey(item.path))) return false;
    return !query || label(item).toLocaleLowerCase(options.locale).includes(query)
      || (options.searchPath !== false && item.path.toLocaleLowerCase(options.locale).includes(query));
  }).sort((a, b) => {
    if (options.sort === "name-desc") return -byName(a, b);
    if (options.sort === "date-desc") {
      const at = timestamp(a), bt = timestamp(b);
      const aKnown = typeof at === "number" && Number.isFinite(at), bKnown = typeof bt === "number" && Number.isFinite(bt);
      if (aKnown !== bKnown) return aKnown ? -1 : 1;
      if (aKnown && bKnown && at !== bt) return bt - at;
    }
    return byName(a, b);
  });
}
