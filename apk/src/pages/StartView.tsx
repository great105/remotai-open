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
    <div className="start-top">{hasServerConfig() && <button className="btn btn-secondary" aria-label="Назад" onClick={goBack}>←</button>}<b>Remotai</b><Link to="/account">Личный кабинет</Link></div>
    <div className="onb-hero">
      <h1>{install ? task === "server" ? "Подключить сервер" : "Подключить компьютер" : "Где работают ваши ИИ-агенты?"}</h1>
      <p>{install
        ? "Установите Remotai там, где работают Claude Code, Codex или другие агенты. После подключения вы сможете давать им задачи с телефона или другого ПК."
        : "Подключите машину с агентами — и продолжайте работу с ними из браузера или приложения. Ваши проекты и окружение остаются на этой машине."}</p>
    </div>
    {install ? <>
      <Link className="start-back" to="/start">← Выбрать другую задачу</Link>
      <ol className="start-steps">
        <li><h2>Установите Remotai на нужную машину</h2><InstallTarget server={task === "server"} os={os} onOSChange={nextOS => {
          const next = new URLSearchParams(params);
          next.set("os", nextOS);
          setParams(next, { replace: true });
        }} /></li>
        <li><h2>Получите код на подключаемой машине</h2><p>{pair.text}</p>
          <details><summary>Код истёк или не появился?</summary><p>{pair.recovery}</p></details>
          {isOnPCPanel() && task === "host" && <Link className="btn btn-secondary" to="/panel">Открыть «Панель ПК»</Link>}
        </li>
        <li><h2>Добавьте машину в свой аккаунт</h2><p>В установленном Remotai нажмите «Подключить к аккаунту в браузере» и войдите на этом же компьютере — код подставится автоматически. Также можно войти с телефона или другого ПК и ввести код в «Моих компьютерах» или отсканировать QR.</p>
          <Link className="btn btn-primary" to={`/infrastructure?add=1&type=${task === "server" ? "server" : "computer"}&back=${encodeURIComponent("/pty?new=1&agent=1")}`}>Перейти к подключению</Link>
        </li>
        <li><h2>Запустите агента и дайте ему задачу</h2><p>Выберите подключённую машину и папку проекта. Окно «Агент» покажет установленные CLI, поможет установить недостающий и выбрать его аккаунт. После запуска введите задачу в терминале — ответ появится здесь же.</p><Link className="start-back" to={`/infrastructure?back=${encodeURIComponent("/pty?new=1&agent=1")}`}>Машина подключена — перейти к агенту</Link></li>
      </ol>
      {task === "server" && <details><summary>Уже подключаетесь к серверу по SSH?</summary><p>Можно пользоваться разделом «SSH-серверы» через подключённый компьютер: Remotai на самом сервере тогда не нужен. Этот компьютер должен оставаться включённым. Установка Remotai прямо на сервер даёт независимый доступ.</p><Link to="/ssh">Открыть «SSH-серверы»</Link></details>}
    </> : <div className="start-choices">
      <Link to="/start?task=host"><strong>На моём компьютере</strong><span>Подключить Windows, macOS или Linux, где лежат проекты и работают агенты.</span></Link>
      <Link to="/start?task=server"><strong>На сервере</strong><span>Подключить Linux / VPS по SSH. Агенты смогут работать на сервере, пока вы вне рабочего места.</span></Link>
      <Link to={`/infrastructure?back=${encodeURIComponent("/pty?new=1&agent=1")}`}><strong>Машина уже подключена</strong><span>Войти, выбрать машину и открыть агента. На устройстве управления достаточно браузера.</span></Link>
      {isOnPCPanel() && <Link to="/panel"><strong>Настроить доступ к этому компьютеру</strong><span>Открыть «Панель ПК»: код подключения, автозапуск и обновления.</span></Link>}
      {/* Четвёртый ход — для того, кто ещё не решил.
          Замер 09.09.2026: 16 визитов с рекламы из 43 КОНЧИЛИСЬ на этом экране.
          Человек нажал на сайте «Начать», получил три предложения установить
          программу и ушёл — изучить продукт отсюда было нечем. Ведём на раздел
          «Что такое Remotai», а не на верх сайта: возвращать в начало круга —
          это не ответ.
          Только в вебе: в приложении и в Telegram сайта под рукой нет, а
          «Панель ПК» открыта на самой машине, где вопрос «что это» не стоит. */}
      {!isNativeApp && !isOnPCPanel() && (
        <a href="/#about"><strong>Пока просто смотрю</strong><span>Коротко о продукте: что это, что нужно и сколько стоит. Ничего устанавливать не надо.</span></a>
      )}
    </div>}
    <p className="onb-hint">Аккаунт Remotai связывает ваши устройства. Аккаунты Claude, ChatGPT и других сервисов подключаются отдельно: Remotai не включает их подписки. Саму машину добавляют по коду или QR с неё.</p>
    {/* Срок пробы — 7 дней с 08.09.2026 (решение владельца, `TRIAL_DAYS=7` на
        бою, то же число на лендинге и в объявлениях). Здесь стояло «30», и это
        был первый экран, который человек видит: обещание больше того, что он
        получит, — худший вид неточности. Кто зарегистрировался раньше, свои 30
        дней сохраняет, поэтому оговорка в скобках, а не молча. */}
    <p className="onb-hint">Локальная работа на своём компьютере — бесплатно. Серверы, SSH/SFTP и облачный доступ — первые 7 дней без карты (у зарегистрировавшихся до 08.09.2026 — 30), затем Про 900 ₽ за 30 дней. Продление вручную, автоматических списаний нет.</p>
  </main>;
}
