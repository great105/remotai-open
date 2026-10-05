import { t } from "@tgcontrol/shared";
import { Link, useSearchParams } from "react-router-dom";
import { InstallTarget, isInstallHandheld } from "../components/InstallTarget";
import { installOS, pairInstructions } from "../installFlow";
import { hasServerConfig, isNativeApp, isOnPCPanel } from "../config";
import { useGoBack } from "../navBack";
import "../onboarding.css";

export function StartView() {
  const [params, setParams] = useSearchParams();
  const goBack = useGoBack();
  const task = params.get("task");
  const install = task === "host" || task === "server";
  const os = installOS(params.get("os"));
  const pair = pairInstructions(os, task === "server", isInstallHandheld());
  return <main className="onb start-page">
    <div className="start-top">{hasServerConfig() && <button className="btn btn-secondary" aria-label={t("agentSessions.back")} onClick={goBack}>←</button>}<b>Remotai</b><Link to="/account">{t("home.trial.btn")}</Link></div>
    <div className="onb-hero">
      <h1>{install ? task === "server" ? t("ui.infrastructureview.m2fd3bdd68a") : t("settings.connectAnotherPc") : t("ui.startview.m72ffe5dfbe")}</h1>
      <p>{install
        ? t("ui.startview.m71c0719bc2")
        : t("ui.startview.m686312f8b9")}</p>
    </div>
    {install ? <>
      <Link className="start-back" to="/start">{t("ui.startview.m5f4b60214f")}</Link>
      <ol className="start-steps">
        <li><h2>{t("ui.startview.me0bb445f3e")}</h2><InstallTarget server={task === "server"} os={os} onOSChange={nextOS => {
          const next = new URLSearchParams(params);
          next.set("os", nextOS);
          setParams(next, { replace: true });
        }} /></li>
        <li><h2>{t("ui.startview.m64676f4225")}</h2><p>{pair.text}</p>
          <details><summary>{t("ui.startview.mc2a0b91eeb")}</summary><p>{pair.recovery}</p></details>
          {isOnPCPanel() && task === "host" && <Link className="btn btn-secondary" to="/panel">{t("ui.infrastructureview.mcf378ef7d0")}</Link>}
        </li>
        <li><h2>{t("ui.startview.ma92a6186b5")}</h2><p>{t("ui.startview.mc1627d4ab2")}</p>
          <Link className="btn btn-primary" to={`/infrastructure?add=1&type=${task === "server" ? "server" : "computer"}&back=${encodeURIComponent("/pty?new=1&agent=1")}`}>{t("ui.startview.m0c52f0c94e")}</Link>
        </li>
        <li><h2>{t("ui.startview.m1cab777226")}</h2><p>{t("ui.startview.m13668d0353")}</p><Link className="start-back" to={`/infrastructure?back=${encodeURIComponent("/pty?new=1&agent=1")}`}>{t("ui.startview.m6bb829f390")}</Link></li>
      </ol>
      {task === "server" && <details><summary>{t("ui.startview.mbc839c3555")}</summary><p>{t("ui.startview.m58decc883b")}</p><Link to="/ssh">{t("ui.startview.m4b7d3668b1")}</Link></details>}
    </> : <div className="start-choices">
      <Link to="/start?task=host"><strong>{t("ui.startview.m8ee959d0e0")}</strong><span>{t("ui.startview.m295f879b37")}</span></Link>
      <Link to="/start?task=server"><strong>{t("ui.startview.md5d4b34a3d")}</strong><span>{t("ui.startview.mf47e66fcfc")}</span></Link>
      <Link to={`/infrastructure?back=${encodeURIComponent("/pty?new=1&agent=1")}`}><strong>{t("ui.startview.m39563ca1d9")}</strong><span>{t("ui.startview.mf10a08fd56")}</span></Link>
      {isOnPCPanel() && <Link to="/panel"><strong>{t("ui.startview.mb1d3fbadb6")}</strong><span>{t("ui.startview.m37d1e5817e")}</span></Link>}
      {/* Четвёртый ход — для того, кто ещё не решил.
          Замер 09.09.2026: 16 визитов с рекламы из 43 КОНЧИЛИСЬ на этом экране.
          Человек нажал на сайте «Начать», получил три предложения установить
          программу и ушёл — изучить продукт отсюда было нечем. Ведём на раздел
          «Что такое Remotai», а не на верх сайта: возвращать в начало круга —
          это не ответ.
          Только в вебе: в приложении и в Telegram сайта под рукой нет, а
          «Панель ПК» открыта на самой машине, где вопрос «что это» не стоит. */}
      {!isNativeApp && !isOnPCPanel() && (
        <a href="/#about"><strong>{t("ui.startview.m6ce15ede0d")}</strong><span>{t("ui.startview.m0e76da8130")}</span></a>
      )}
    </div>}
    <p className="onb-hint">{t("ui.startview.m0ccdef5e84")}</p>
    {/* Срок пробы — 7 дней с 08.09.2026 (решение владельца, `TRIAL_DAYS=7` на
        бою, то же число на лендинге и в объявлениях). Здесь стояло «30», и это
        был первый экран, который человек видит: обещание больше того, что он
        получит, — худший вид неточности. Кто зарегистрировался раньше, свои 30
        дней сохраняет, поэтому оговорка в скобках, а не молча. */}
    <p className="onb-hint">{t("ui.startview.ma448da68ca")}</p>
  </main>;
}
