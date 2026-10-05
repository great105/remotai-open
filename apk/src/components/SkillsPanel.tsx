import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import {
  ApiError, confirmDialog, copySkill, copyTargets, defaultInstallTargets, deleteSkill, filterSkills,
  getSkills, installConflicts, installSkillFromGitHub, installSkillFromZip, looksLikeGitHubUrl, mapApiError,
  recentDeleted, restoreSkill, skillErrorKey, skillLocationTitle, summarizeResults, t, useToast, writableLocations,
  SKILL_ZIP_MAX_BYTES,
  type FoundSkill, type Skill, type SkillBackup, type SkillInstallReport, type SkillLocation, type SkillsState,
} from "@tgcontrol/shared";
import { haptic, hapticSuccess } from "../telegram";
import "./SkillsPanel.css";

/**
 * Скиллы агентов (раздел «Агенты» → «Скиллы»).
 *
 * Что здесь можно: посмотреть, что стоит у каждого агента; поставить скилл по
 * ссылке GitHub или из ZIP; скопировать скилл другому агенту; удалить — с
 * возвратом, потому что сервер не удаляет, а уносит в резервную копию.
 *
 * Правила (кому можно ставить, где конфликт, как назвать место) — в
 * packages/shared/src/skills.ts, здесь только показ.
 */

/** Сколько строк показываем сразу: у владельца у Claude 60+ скиллов, и
 *  длинный свиток хоронит под собой всё, что ниже на экране. */
const FIRST_ROWS = 8;

function errorText(e: unknown): string {
  if (e instanceof ApiError) {
    const key = skillErrorKey(e.code);
    if (key) return t(key);
  }
  return mapApiError(e);
}

function placeTitle(loc: SkillLocation | undefined): string {
  return loc ? skillLocationTitle(loc, t("skills.plugins")) : "";
}

function resultLine(rep: SkillInstallReport | { results: SkillInstallReport["results"] }): string {
  const s = summarizeResults(rep.results);
  const parts: string[] = [];
  if (s.installed) parts.push(t("skills.res.installed", { n: s.installed }));
  if (s.replaced) parts.push(t("skills.res.replaced", { n: s.replaced }));
  if (s.exists) parts.push(t("skills.res.exists", { n: s.exists }));
  if (s.same) parts.push(t("skills.res.same", { n: s.same }));
  if (s.errors.length) parts.push(t("skills.res.errors", { n: s.errors.length, why: s.errors[0] }));
  return parts.join(", ");
}

function Chevron({ open }: { open: boolean }) {
  return (
    <svg className={`skills-chev${open ? " open" : ""}`} width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
      <path d="M4 6l4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function Check() {
  return (
    <svg className="skills-check" width="14" height="14" viewBox="0 0 14 14" aria-hidden="true">
      <path d="M3 7.5l2.5 2.5L11 4.5" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** Переключатель-«чип» с галочкой: выбор агентов и скиллов. */
function ToggleChip({ on, onToggle, disabled, children, hint }: {
  on: boolean; onToggle: () => void; disabled?: boolean; children: React.ReactNode; hint?: string;
}) {
  return (
    <button
      type="button"
      className={`skills-chip${on ? " on" : ""}`}
      aria-pressed={on}
      disabled={disabled}
      onClick={() => { haptic(); onToggle(); }}
    >
      <span className="skills-chip-box" aria-hidden="true">{on && <Check />}</span>
      <span className="skills-chip-text">{children}</span>
      {hint && <small className="skills-chip-hint">{hint}</small>}
    </button>
  );
}

// ── Установка ────────────────────────────────────────────────────────────────

type Source = "link" | "zip";

function InstallPanel({ locations, onDone, onClose }: {
  locations: SkillLocation[];
  onDone: () => void;
  onClose: () => void;
}) {
  const [source, setSource] = useState<Source>("link");
  const [url, setUrl] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [found, setFound] = useState<FoundSkill[] | null>(null);
  const [picked, setPicked] = useState<string[]>([]);
  const [targets, setTargets] = useState<string[]>(() => defaultInstallTargets(locations));
  const [replace, setReplace] = useState(false);
  const [busy, setBusy] = useState<"find" | "install" | null>(null);
  const [error, setError] = useState("");
  const [result, setResult] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);
  const urlId = useId();
  const writable = writableLocations(locations);

  const reset = () => { setFound(null); setPicked([]); setError(""); setResult(""); setReplace(false); };

  const scan = async (src: Source, f: File | null) => {
    reset();
    if (src === "link" && !looksLikeGitHubUrl(url)) { setError(t("skills.linkInvalid")); return; }
    if (src === "zip" && f && f.size > SKILL_ZIP_MAX_BYTES) { setError(t("skills.zipTooBig")); return; }
    if (src === "zip" && !f) return;
    setBusy("find");
    try {
      const opts = { targets: [], dryRun: true };
      const rep = src === "link" ? await installSkillFromGitHub(url.trim(), opts) : await installSkillFromZip(f!, opts);
      setFound(rep.found);
      setPicked(rep.found.filter((x) => !x.skip).map((x) => x.name));
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(null);
    }
  };

  const conflicts = found ? installConflicts(locations, targets, picked) : [];

  const install = async () => {
    setError(""); setResult("");
    if (!picked.length) { setError(t("skills.pickSkills")); return; }
    if (!targets.length) { setError(t("skills.pickTargets")); return; }
    setBusy("install");
    try {
      const opts = { targets, replace, only: picked };
      const rep = source === "link" ? await installSkillFromGitHub(url.trim(), opts) : await installSkillFromZip(file!, opts);
      hapticSuccess();
      setResult(resultLine(rep));
      setFound(null);
      onDone();
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="skills-install" role="group" aria-label={t("skills.install")}>
      <div className="skills-seg" role="radiogroup" aria-label={t("skills.install")}>
        {(["link", "zip"] as Source[]).map((s) => (
          <button
            key={s}
            type="button"
            role="radio"
            aria-checked={source === s}
            className={`skills-seg-btn${source === s ? " on" : ""}`}
            onClick={() => { haptic(); setSource(s); reset(); }}
          >
            {s === "link" ? t("skills.fromLink") : t("skills.fromZip")}
          </button>
        ))}
      </div>

      {source === "link" ? (
        <form
          className="skills-link-form"
          onSubmit={(e) => { e.preventDefault(); void scan("link", null); }}
        >
          <label className="skills-label" htmlFor={urlId}>{t("skills.linkLabel")}</label>
          <div className="skills-link-row">
            <input
              id={urlId}
              className="skills-input"
              type="url"
              inputMode="url"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              placeholder={t("skills.linkPlaceholder")}
              value={url}
              onChange={(e) => { setUrl(e.target.value); if (found || error) reset(); }}
            />
            <button type="submit" className="btn btn-secondary skills-find" disabled={busy !== null || !url.trim()}>
              {busy === "find" ? t("skills.finding") : t("skills.find")}
            </button>
          </div>
          <p className="skills-hint">{t("skills.linkHint")}</p>
        </form>
      ) : (
        <div className="skills-zip">
          <input
            ref={fileRef}
            type="file"
            accept=".zip,application/zip,application/x-zip-compressed"
            className="skills-file"
            tabIndex={-1}
            aria-hidden="true"
            onChange={(e) => {
              const f = e.target.files?.[0] ?? null;
              e.target.value = "";
              setFile(f);
              if (f) void scan("zip", f);
            }}
          />
          <button type="button" className="btn btn-secondary skills-zip-btn" disabled={busy !== null} onClick={() => { haptic(); fileRef.current?.click(); }}>
            {busy === "find" ? t("skills.finding") : file ? t("skills.pickOtherZip") : t("skills.pickZip")}
          </button>
          {file && <span className="skills-zip-name">{file.name}</span>}
          <p className="skills-hint">{t("skills.zipHint")}</p>
        </div>
      )}

      {found && (
        <div className="skills-found">
          <h3 className="skills-sub">{t("skills.found", { n: found.length })}</h3>
          <div className="skills-found-list">
            {found.map((f) => (
              <ToggleChip
                key={f.name}
                on={picked.includes(f.name)}
                disabled={!!f.skip}
                onToggle={() => setPicked((p) => (p.includes(f.name) ? p.filter((x) => x !== f.name) : [...p, f.name]))}
                hint={f.skip ? t("skills.skip", { reason: f.skip }) : f.description}
              >
                {f.name}
              </ToggleChip>
            ))}
          </div>

          <h3 className="skills-sub">{t("skills.whereTo")}</h3>
          <div className="skills-targets">
            {writable.map((l) => (
              <ToggleChip
                key={l.id}
                on={targets.includes(l.id)}
                onToggle={() => setTargets((p) => (p.includes(l.id) ? p.filter((x) => x !== l.id) : [...p, l.id]))}
              >
                {placeTitle(l)}
              </ToggleChip>
            ))}
          </div>

          {conflicts.length > 0 && (
            <div className="skills-conflict">
              <p>{t("skills.alreadyThere", { names: conflicts.join(", ") })}</p>
              <label className="skills-replace">
                <input type="checkbox" checked={replace} onChange={(e) => { haptic(); setReplace(e.target.checked); }} />
                <span>{t("skills.replace")}</span>
              </label>
            </div>
          )}

          <button
            type="button"
            className="btn btn-primary skills-go"
            disabled={busy !== null}
            onClick={() => void install()}
          >
            {busy === "install" ? t("skills.installing") : t("skills.installN", { n: picked.length })}
          </button>
        </div>
      )}

      {error && <p className="skills-error" role="alert">{error}</p>}
      {result && (
        <div className="skills-result" role="status">
          <p><b>{t("skills.res.head")}:</b> {result}.</p>
          <p className="skills-hint">{t("skills.newChat")}</p>
        </div>
      )}

      <button type="button" className="skills-textbtn" onClick={() => { haptic(); onClose(); }}>
        {t("skills.installClose")}
      </button>
    </div>
  );
}

// ── Строка скилла ────────────────────────────────────────────────────────────

function SkillRow({ skill, loc, locations, open, onToggle, onCopy, onDelete, busy }: {
  skill: Skill;
  loc: SkillLocation;
  locations: SkillLocation[];
  open: boolean;
  onToggle: () => void;
  onCopy: (to: SkillLocation, has: boolean) => void;
  onDelete: () => void;
  busy: boolean;
}) {
  const panelId = useId();
  const targets = open ? copyTargets(locations, loc.id, skill.name) : [];
  const canAct = !skill.readonly && loc.kind !== "plugins";
  const badge = skill.source === "system" ? t("skills.badge.system")
    : skill.source ? skill.source
    : skill.link ? t("skills.badge.link") : "";
  return (
    <li className={`skill-row${open ? " open" : ""}`}>
      <button type="button" className="skill-row-main" aria-expanded={open} aria-controls={panelId} onClick={() => { haptic(); onToggle(); }}>
        <span className="skill-row-text">
          <span className="skill-row-name">
            {skill.name}
            {badge && <span className="skill-badge">{badge}</span>}
          </span>
          <span className={`skill-row-desc${open ? " full" : ""}`}>{skill.description || t("skills.noDescription")}</span>
        </span>
        <Chevron open={open} />
      </button>
      {open && (
        <div className="skill-row-panel" id={panelId}>
          {!canAct && <p className="skills-hint">{loc.kind === "plugins" ? t("skills.pluginsNote") : t("skills.readonlyRow")}</p>}
          {canAct && (
            <>
              <div className="skill-row-label">{t("skills.copyTo")}</div>
              {targets.length ? (
                <div className="skill-row-copy">
                  {targets.map(({ location, has }) => (
                    <button
                      key={location.id}
                      type="button"
                      className="btn btn-secondary btn-sm skills-copy-btn"
                      disabled={busy}
                      onClick={() => { haptic(); onCopy(location, has); }}
                    >
                      {placeTitle(location)}
                      {has && <small>{t("skills.copyHas")}</small>}
                    </button>
                  ))}
                </div>
              ) : (
                <p className="skills-hint">{t("skills.copyNowhere")}</p>
              )}
              <button type="button" className="skills-delete" disabled={busy} onClick={() => { haptic(); onDelete(); }}>
                {t("skills.delete")}
              </button>
            </>
          )}
        </div>
      )}
    </li>
  );
}

// ── Панель ───────────────────────────────────────────────────────────────────

export function SkillsPanel() {
  const { toast, toastSuccess, toastError } = useToast();
  const [state, setState] = useState<SkillsState | null>(null);
  const [loadError, setLoadError] = useState<"" | "update" | "fail">("");
  const [current, setCurrent] = useState("");
  const [query, setQuery] = useState("");
  const [openRow, setOpenRow] = useState("");
  const [showAll, setShowAll] = useState(false);
  const [installOpen, setInstallOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const searchId = useId();

  const load = useCallback(async () => {
    try {
      const next = await getSkills();
      setState(next);
      setLoadError("");
      setCurrent((c) => (c && next.locations.some((l) => l.id === c) ? c : next.locations[0]?.id ?? ""));
    } catch (e) {
      setLoadError(e instanceof ApiError && (e.status === 404 || e.status === 405) ? "update" : "fail");
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const locations = state?.locations ?? [];
  const loc = locations.find((l) => l.id === current);
  const visible = useMemo(() => filterSkills(loc?.skills ?? [], query), [loc, query]);
  const rows = showAll || query ? visible : visible.slice(0, FIRST_ROWS);
  const recent = recentDeleted(state?.backups ?? []);

  const restore = async (b: Pick<SkillBackup, "id" | "location" | "name">, replace = false): Promise<void> => {
    try {
      await restoreSkill(b, replace);
      hapticSuccess();
      toastSuccess(t("skills.restored", { name: b.name }));
    } catch (e) {
      if (!replace && e instanceof ApiError && e.code === "exists") {
        const ok = await confirmDialog(t("skills.restoreConflict", { name: b.name }), { confirmText: t("skills.restoreReplace") });
        if (ok) return restore(b, true);
        return;
      }
      toastError(errorText(e));
    } finally {
      void load();
    }
  };

  const remove = async (skill: Skill, from: SkillLocation) => {
    const where = placeTitle(from);
    const shared = from.shared_with?.length ? ` ${t("skills.deleteShared", { names: from.shared_with.join(", ") })}` : "";
    const ok = await confirmDialog(t("skills.deleteConfirm", { name: skill.name, where }) + shared, {
      danger: true, confirmText: t("skills.delete"),
    });
    if (!ok) return;
    setBusy(true);
    try {
      const res = await deleteSkill(from.id, skill.name);
      hapticSuccess();
      setOpenRow("");
      toast(t("skills.deleted", { name: skill.name }), "success", {
        durationMs: 8000,
        action: { label: t("skills.restore"), onClick: () => void restore(res.backup) },
      });
    } catch (e) {
      toastError(errorText(e));
    } finally {
      setBusy(false);
      void load();
    }
  };

  const copy = async (skill: Skill, from: SkillLocation, to: SkillLocation, has: boolean) => {
    const where = placeTitle(to);
    if (has) {
      const ok = await confirmDialog(t("skills.copyExists", { name: skill.name, where }), { confirmText: t("skills.copyReplace") });
      if (!ok) return;
    }
    setBusy(true);
    try {
      const res = await copySkill(skill.name, from.id, [to.id], has);
      const s = summarizeResults(res.results);
      if (s.errors.length) toastError(s.errors[0]);
      else { hapticSuccess(); toastSuccess(t("skills.copied", { name: skill.name, where })); }
    } catch (e) {
      toastError(errorText(e));
    } finally {
      setBusy(false);
      void load();
    }
  };

  if (loadError === "update") return <p className="skills-hint skills-pad">{t("skills.needsUpdate")}</p>;
  if (loadError && !state) {
    return (
      <div className="skills-state">
        <p className="skills-error">{t("skills.loadError")}</p>
        <button type="button" className="btn btn-secondary" onClick={() => { haptic(); void load(); }}>{t("skills.retry")}</button>
      </div>
    );
  }
  if (!state) {
    return (
      <div className="skills" aria-busy="true">
        <div className="skills-skel" /><div className="skills-skel short" /><div className="skills-skel" />
      </div>
    );
  }
  if (!locations.length) return <p className="skills-hint skills-pad">{t("skills.noAgents")}</p>;

  return (
    <div className="skills">
      <p className="skills-intro">{t("skills.intro")}</p>

      {writableLocations(locations).length > 0 && (
        installOpen ? (
          <InstallPanel locations={locations} onDone={() => void load()} onClose={() => setInstallOpen(false)} />
        ) : (
          <button type="button" className="btn btn-primary skills-open-install" onClick={() => { haptic(); setInstallOpen(true); }}>
            {t("skills.install")}
          </button>
        )
      )}

      <div className="skills-places" role="tablist" aria-label={t("skills.title")}>
        {locations.map((l) => (
          <button
            key={l.id}
            type="button"
            role="tab"
            aria-selected={l.id === current}
            className={`skills-place${l.id === current ? " on" : ""}`}
            onClick={() => { haptic(); setCurrent(l.id); setOpenRow(""); setShowAll(false); }}
          >
            <span>{placeTitle(l)}</span>
            <span className="skills-place-n">{l.skills.length}</span>
          </button>
        ))}
      </div>

      {loc && (
        <div className="skills-body" role="tabpanel">
          {!!loc.shared_with?.length && (
            <p className="skills-hint">{t("skills.shared", { n: loc.shared_with.length, names: loc.shared_with.join(", ") })}</p>
          )}
          {loc.kind === "plugins" && <p className="skills-hint">{t("skills.pluginsNote")}</p>}
          {loc.skills.length > FIRST_ROWS && (
            <>
              <label className="skills-visually-hidden" htmlFor={searchId}>{t("skills.search")}</label>
              <input
                id={searchId}
                className="skills-input skills-search"
                type="search"
                placeholder={t("skills.search")}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </>
          )}
          {loc.skills.length === 0 ? (
            <p className="skills-hint skills-pad">{t("skills.empty")}</p>
          ) : rows.length === 0 ? (
            <p className="skills-hint skills-pad">{t("skills.noMatch")}</p>
          ) : (
            <ul className="skills-list">
              {rows.map((s) => {
                const key = `${s.source ?? ""}/${s.name}`;
                return (
                  <SkillRow
                    key={key}
                    skill={s}
                    loc={loc}
                    locations={locations}
                    open={openRow === key}
                    busy={busy}
                    onToggle={() => setOpenRow((o) => (o === key ? "" : key))}
                    onCopy={(to, has) => void copy(s, loc, to, has)}
                    onDelete={() => void remove(s, loc)}
                  />
                );
              })}
            </ul>
          )}
          {!query && visible.length > FIRST_ROWS && (
            <button type="button" className="skills-textbtn" onClick={() => { haptic(); setShowAll((v) => !v); }}>
              {showAll ? t("skills.showLess") : t("skills.showAll", { n: visible.length })}
            </button>
          )}
        </div>
      )}

      {recent.length > 0 && (
        <div className="skills-recent">
          <h3 className="skills-sub">{t("skills.recent")}</h3>
          <ul className="skills-recent-list">
            {recent.map((b) => (
              <li key={`${b.id}/${b.location}/${b.name}`} className="skills-recent-row">
                <span className="skills-recent-text">
                  <b>{b.name}</b>
                  <small>{t("skills.recentFrom", {
                    where: placeTitle(locations.find((l) => l.id === b.location)) || b.location,
                    when: new Date(b.at * 1000).toLocaleString("ru-RU", { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }),
                  })}</small>
                </span>
                <button type="button" className="btn btn-secondary btn-sm" onClick={() => { haptic(); void restore(b); }}>
                  {t("skills.restore")}
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}
