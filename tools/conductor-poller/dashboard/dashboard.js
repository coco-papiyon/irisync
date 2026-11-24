"use strict";

const byId = (id) => document.getElementById(id);
const formatTime = new Intl.DateTimeFormat("ja-JP", { dateStyle: "short", timeStyle: "medium" });
const labels = {
  OPEN: "オープン", CLOSED: "クローズ", MERGED: "マージ済み", Draft: "ドラフト",
  SUCCESS: "成功", FAILURE: "失敗", ERROR: "エラー", PENDING: "進行中", EXPECTED: "待機中",
  APPROVED: "承認済み", CHANGES_REQUESTED: "修正依頼あり", REVIEW_REQUIRED: "レビュー待ち",
  COMMENTED: "コメント", DISMISSED: "レビュー取消",
  smoke: "スモークテスト", design: "設計", implementation: "実装", review: "レビュー",
  "implementation attempt": "実装の試行", "pr fix": "PR 修正", "merge check": "マージ状況の確認",
  running: "実行中", succeeded: "成功", failed: "失敗", interrupted: "中断", timed_out: "タイムアウト",
  approved: "承認済み", validated: "ローカル検証済み", published: "PR 作成済み",
  "awaiting approval": "設計承認待ち", "changes requested": "修正が必要", recorded: "試行を記録済み"
};
const good = new Set(["Done", "SUCCESS", "MERGED", "APPROVED", "succeeded", "approved", "published", "validated"]);
const bad = new Set(["Blocked", "FAILURE", "ERROR", "CHANGES_REQUESTED", "failed", "timed_out", "changes requested"]);
const wait = new Set(["Pending", "Design Review", "PR Review", "REVIEW_REQUIRED", "EXPECTED", "awaiting approval"]);
const active = new Set(["Ready", "Designing", "Implementing", "PR Fixing", "Merge Ready", "OPEN", "PENDING", "running"]);
let snapshot;
let resultsByIssue = new Map();
let logRequest = 0;
let selectedItem;
let approval;
let discussionRequest = 0;
let sendingApproval = false;
let sendingRetry = false;
let settingsRevision;
let settingsRequest = 0;
let savingSettings = false;

function node(tag, text, className) {
  const element = document.createElement(tag);
  if (text !== undefined) element.textContent = text;
  if (className) element.className = className;
  return element;
}

function badge(status) {
  if (!status) return node("span", "—", "empty");
  const style = good.has(status) ? "good" : bad.has(status) ? "bad" : wait.has(status) ? "wait" : active.has(status) ? "active" : "";
  return node("span", labels[status] || status, "badge " + style);
}

function link(label, target) {
  const element = node("a", label);
  element.href = target;
  element.target = "_blank";
  element.rel = "noopener noreferrer";
  return element;
}

function itemButton(kind, item) {
  const element = node("button", item.Title || ("#" + item.Number), "item-link");
  element.type = "button";
  if (item.Title) element.prepend(node("span", "#" + item.Number, "issue-number"));
  element.addEventListener("click", () => {
    selectedItem = { kind, number: item.Number };
    message("item-retry-note", "");
    if (renderItemDetail()) {
      byId("item-dialog").showModal();
      loadDiscussion();
    }
  });
  return element;
}

function timeText(value) {
  if (!value) return "—";
  return formatTime.format(new Date(value));
}

function fillTable(id, rows, columns, emptyMessage) {
  const tbody = byId(id + "-body");
  const fragment = document.createDocumentFragment();
  for (const row of rows) {
    const tr = node("tr");
    for (const value of row) {
      const td = node("td");
      td.append(value instanceof Node ? value : String(value));
      tr.append(td);
    }
    fragment.append(tr);
  }
  if (!rows.length) {
    const td = node("td", emptyMessage, "empty");
    td.colSpan = columns;
    const tr = node("tr");
    tr.append(td);
    fragment.append(tr);
  }
  tbody.replaceChildren(fragment);
}

function message(id, text) {
  const element = byId(id);
  element.hidden = !text;
  element.textContent = text || "";
}

function renderIssues() {
  if (!snapshot) return;
  const key = byId("search").value.trim().toLowerCase();
  const select = byId("status-filter");
  const selected = select.value;
  const issues = snapshot.Issues || [];
  const statuses = [...new Set(issues.map((issue) => issue.Status).filter(Boolean))].sort();
  select.replaceChildren(new Option("全ステータス", ""), ...statuses.map((status) => new Option(status, status)));
  if (statuses.includes(selected)) select.value = selected;
  const filtered = issues.filter((issue) => (!select.value || issue.Status === select.value) &&
    ("#" + issue.Number + " " + issue.Title).toLowerCase().includes(key));
  filtered.sort((a, b) => (Date.parse(b.UpdatedAt) || 0) - (Date.parse(a.UpdatedAt) || 0) || b.Number - a.Number);
  fillTable("issues", filtered.map((issue) => {
    const results = resultsByIssue.get(issue.Number) || [];
    return [
      itemButton("issue", issue), badge(issue.Status),
      issueResultSummary(results.find((result) => result.Outcome === "running")),
      issueResultSummary(results.find((result) => result.Outcome !== "running"), true)
    ];
  }), 4, "表示する Issue はありません");
}

function groupIssueResults(data) {
  const issueNumbersByPR = new Map((data.PRs || []).map((pr) => [pr.Number, pr.Issues || []]));
  const grouped = new Map();
  for (const result of data.Results || []) {
    const numbers = new Set([result.Issue, ...(issueNumbersByPR.get(result.PR) || [])]);
    for (const number of numbers) {
      if (!number) continue;
      if (!grouped.has(number)) grouped.set(number, []);
      grouped.get(number).push(result);
    }
  }
  for (const results of grouped.values()) {
    results.sort((a, b) => (Date.parse(b.Timestamp) || 0) - (Date.parse(a.Timestamp) || 0));
  }
  return grouped;
}

function issueResultSummary(result, showTimestamp = false) {
  if (!result) return node("span", "—", "empty");
  const summary = node("div", undefined, "issue-result");
  const phase = labels[result.Phase] || result.Phase;
  summary.append(node("span", result.Attempts ? phase + " " + result.Attempts + "回目" : phase), badge(result.Outcome));
  if (result.Feedback) summary.append(feedbackButton(result));
  if (showTimestamp && result.Timestamp) {
    summary.append(node("span", timeText(result.Timestamp), "result-time"));
  }
  if (result.LogPath) summary.append(logButton(result.LogPath, "ログ"));
  return summary;
}

function feedbackButton(result) {
  const button = node("button", "AI レビュー結果", "log-link");
  button.type = "button";
  button.addEventListener("click", () => {
    const context = [
      result.Issue ? "Issue #" + result.Issue : "",
      result.PR ? "PR #" + result.PR : "",
      labels[result.Phase] || result.Phase,
      result.Attempts ? result.Attempts + "回目" : ""
    ].filter(Boolean);
    byId("feedback-context").textContent = context.join(" · ");
    byId("feedback-content").textContent = result.Feedback;
    byId("feedback-dialog").showModal();
  });
  return button;
}

function logButton(path, label) {
  const button = node("button", label, "log-link");
  button.type = "button";
  button.addEventListener("click", () => openLog(path));
  return button;
}

function resultRow(result) {
  const target = node("span");
  const origin = "https://github.com/" + snapshot.Repository;
  if (result.Issue) target.append(link("Issue #" + result.Issue, origin + "/issues/" + result.Issue));
  if (result.Issue && result.PR) target.append(" / ");
  if (result.PR) target.append(link("PR #" + result.PR, origin + "/pull/" + result.PR));
  const phase = labels[result.Phase] || result.Phase;
  const outcome = node("div", undefined, "result-outcome");
  outcome.append(badge(result.Outcome));
  if (result.Feedback) outcome.append(feedbackButton(result));
  return [phase, target.firstChild ? target : "—", outcome, result.Attempts || "—",
    timeText(result.Timestamp), result.LogPath ? logButton(result.LogPath, "開く") : "—"];
}

function renderItemDetail() {
  if (!selectedItem) return false;
  const { kind, number } = selectedItem;
  const prs = snapshot.PRs || [];
  const item = (kind === "issue" ? snapshot.Issues || [] : prs).find((entry) => entry.Number === number);
  const dialog = byId("item-dialog");
  if (!item) {
    if (dialog.open) dialog.close();
    return false;
  }
  byId("item-title").textContent = (kind === "issue" ? "Issue" : "PR") + " #" + number + " " + item.Title;
  byId("item-github").href = item.URL;
  const retry = byId("item-retry");
  retry.hidden = kind !== "issue" || item.Status !== "Blocked" || !item.Retry;
  retry.disabled = sendingRetry;
  byId("item-discussion-title").textContent = kind === "issue" ? "GitHub のコメント" : "GitHub のコメント・レビュー";
  const status = byId("item-status");
  if (kind === "issue") {
    status.replaceChildren(badge(item.Status));
  } else {
    status.replaceChildren(badge(item.IsDraft ? "Draft" : item.State), badge(item.Checks), badge(item.ReviewDecision));
    for (const issue of item.Issues || []) {
      status.append(link("関連 Issue #" + issue, "https://github.com/" + snapshot.Repository + "/issues/" + issue));
    }
  }
  const results = kind === "issue" ? resultsByIssue.get(number) || [] : (snapshot.Results || []).filter((entry) =>
    entry.PR === number || (entry.PR === 0 && item.Issues?.includes(entry.Issue) && ["design", "smoke"].includes(entry.Phase)));
  message("item-results-message", [snapshot.Errors.results, snapshot.Errors.logs].filter(Boolean).join(" "));
  fillTable("item-results", results.map(resultRow), 6, "関連する実行結果はありません");
  return true;
}

async function retryImplementation() {
  const target = selectedItem;
  if (sendingRetry || target?.kind !== "issue") return;
  const item = (snapshot.Issues || []).find((issue) => issue.Number === target.number);
  if (item?.Status !== "Blocked" || !item.Retry) return;
  if (!window.confirm("Issue #" + item.Number + " の実装を再開しますか？")) return;
  const button = byId("item-retry");
  sendingRetry = true;
  button.disabled = true;
  message("item-retry-note", "実装の再開を登録しています…");
  try {
    const response = await fetch("/api/retry-implementation", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ Number: item.Number, ...item.Retry })
    });
    if (!response.ok) {
      const data = await response.json();
      throw new Error(data.Message || "実装を再開できませんでした。");
    }
    delete item.Retry;
    if (selectedItem === target) {
      button.hidden = true;
      message("item-retry-note", "再開を登録しました。次のポーリングで実装を続けます。");
    }
    refresh();
  } catch (error) {
    if (selectedItem === target) message("item-retry-note", error.message);
  } finally {
    sendingRetry = false;
    button.disabled = false;
  }
}

function commentBody(text) {
  const body = node("p", undefined, "comment-body");
  let from = 0;
  for (const match of text.matchAll(/\[([^\]\r\n]+)\]\((https:\/\/[^\s)]+)\)/g)) {
    const target = safeWebURL(match[2]);
    if (!target) continue;
    body.append(text.slice(from, match.index), link(match[1], target));
    from = match.index + match[0].length;
  }
  body.append(text.slice(from));
  return body;
}

function safeWebURL(value) {
  try {
    const url = new URL(value);
    if (url.protocol === "https:" && url.username === "" && url.password === "") return url.href;
  } catch {
    return undefined;
  }
}

function renderDiscussion(data) {
  const comments = (data.Entries || []).map((entry) => {
    const comment = node("article");
    const metadata = node("div", undefined, "comment-meta");
    metadata.append(node("strong", entry.Author || "GitHub ユーザー"));
    const target = safeWebURL(entry.URL);
    metadata.append(target ? link(timeText(entry.At), target) : node("span", timeText(entry.At)));
    if (entry.State) metadata.append(badge(entry.State));
    comment.append(metadata);
    if (entry.Body) comment.append(commentBody(entry.Body));
    return comment;
  });
  byId("item-comments").replaceChildren(...comments);
  message("item-discussion-status", comments.length ? "" : "コメントはまだありません。");
  approval = data.Approval;
  const target = byId("item-approval-target");
  target.replaceChildren();
  const url = safeWebURL(approval.URL);
  if (url) {
    const label = selectedItem.kind === "issue" ? "設計書" : "PR コミット";
    target.append(link(label + " " + approval.Revision.slice(0, 12), url));
  }
  if (approval.Author) target.append(node("p", "承認者 " + approval.Author));
  const button = byId("item-approve");
  button.textContent = selectedItem.kind === "issue" ? "設計を承認" : "PR を承認";
  button.hidden = !approval.Revision || !!approval.Reason;
  button.disabled = sendingApproval;
  message("item-approval-note", approval.Reason);
}

async function loadDiscussion() {
  if (!selectedItem) return;
  const { kind, number } = selectedItem;
  const request = ++discussionRequest;
  approval = undefined;
  byId("item-approve").hidden = true;
  byId("item-approval-target").replaceChildren();
  byId("item-comments").replaceChildren();
  byId("item-refresh").disabled = true;
  message("item-approval-note", "");
  message("item-discussion-status", "GitHub のコメントを読み込んでいます…");
  try {
    const response = await fetch("/api/item?" + new URLSearchParams({ kind, number }), { cache: "no-store" });
    const data = await response.json();
    if (request !== discussionRequest || !byId("item-dialog").open) return;
    if (!response.ok) throw new Error(data.Message || "GitHub のコメントを取得できませんでした。");
    renderDiscussion(data);
  } catch (error) {
    if (request === discussionRequest) message("item-discussion-status", error.message);
  } finally {
    if (request === discussionRequest) byId("item-refresh").disabled = false;
  }
}

async function submitApproval() {
  if (!selectedItem || !approval?.Revision || approval.Reason || sendingApproval) return;
  const target = selectedItem;
  const { kind, number } = target;
  const { Revision: revision, Author: author } = approval;
  const label = kind === "issue" ? "Issue #" + number + " の設計" : "PR #" + number;
  if (!window.confirm(label + " (" + revision.slice(0, 12) + ") を " + author + " として承認しますか？")) return;
  sendingApproval = true;
  byId("item-approve").disabled = true;
  byId("item-refresh").disabled = true;
  message("item-approval-note", "GitHub に承認を投稿しています…");
  let result;
  try {
    const response = await fetch("/api/approve", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind, number, revision, author })
    });
    if (!response.ok) {
      const data = await response.json();
      throw new Error(data.Message || "承認の投稿に失敗しました。");
    }
    result = "GitHub に承認を投稿しました。";
  } catch (error) {
    result = error.message;
  } finally {
    sendingApproval = false;
    byId("item-approve").disabled = false;
  }
  if (selectedItem === target && byId("item-dialog").open) {
    await loadDiscussion();
    if (selectedItem === target) message("item-approval-note", result);
  }
}

const settingsFields = [
  "DesignApproverIDs", "DashboardApproverLogin", "PollInterval", "AutoReady", "TestIssueNumbers",
  "DesignIssueNumbers", "ImplementationIssueNumbers"
];

async function openSettings() {
  const dialog = byId("settings-dialog");
  const form = byId("settings-form");
  const request = ++settingsRequest;
  settingsRevision = undefined;
  form.reset();
  byId("settings-fields").disabled = true;
  byId("settings-save").disabled = true;
  byId("settings-file").textContent = "";
  byId("settings-login-help").textContent = "";
  message("settings-status", "設定を読み込んでいます…");
  dialog.showModal();
  try {
    const response = await fetch("/api/settings", { cache: "no-store" });
    const data = await response.json();
    if (request !== settingsRequest || !dialog.open) return;
    if (!response.ok) throw new Error(data.Message || "設定を読み取れませんでした。");
    for (const key of settingsFields) {
      const input = form.elements.namedItem(key);
      if (input.type === "checkbox") input.checked = data[key];
      else input.value = data[key];
    }
    settingsRevision = data.Revision;
    byId("settings-file").textContent = "保存先 " + data.FileName;
    byId("settings-login-help").textContent = "空欄の場合は " + data.DefaultApproverLogin + " の保存済み認証を使います。";
    byId("settings-fields").disabled = false;
    byId("settings-save").disabled = false;
    message("settings-status", "");
  } catch (error) {
    if (request === settingsRequest && dialog.open) message("settings-status", error.message);
  }
}

async function saveSettings(event) {
  event.preventDefault();
  if (!settingsRevision || savingSettings) return;
  const form = byId("settings-form");
  const settings = { Revision: settingsRevision };
  for (const key of settingsFields) {
    const input = form.elements.namedItem(key);
    settings[key] = input.type === "checkbox" ? input.checked : input.value;
  }
  savingSettings = true;
  byId("settings-fields").disabled = true;
  byId("settings-save").disabled = true;
  byId("settings-close").disabled = true;
  message("settings-status", "保存しています…");
  try {
    const response = await fetch("/api/settings", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify(settings)
    });
    const data = await response.json();
    if (!response.ok) throw new Error(data.Message || "設定を保存できませんでした。");
    settingsRevision = data.Revision;
    message("settings-status", data.Message);
  } catch (error) {
    message("settings-status", error.message);
  } finally {
    savingSettings = false;
    byId("settings-fields").disabled = false;
    byId("settings-save").disabled = false;
    byId("settings-close").disabled = false;
  }
}

async function openLog(path) {
  const request = ++logRequest;
  const dialog = byId("log-dialog");
  byId("log-title").textContent = path;
  byId("log-content").textContent = "読み込んでいます…";
  message("log-note", "");
  if (!dialog.open) dialog.showModal();
  try {
    const response = await fetch("/api/log?path=" + encodeURIComponent(path));
    if (!response.ok) throw new Error("log unavailable");
    const log = await response.json();
    if (request !== logRequest || !dialog.open) return;
    byId("log-content").textContent = log.Text || "ログは空です";
    message("log-note", log.Truncated ? "ログの末尾 128 KiB を表示しています。" : "");
  } catch {
    if (request === logRequest && dialog.open) byId("log-content").textContent = "ログを読み取れませんでした。";
  }
}

function renderDashboard(data) {
  snapshot = data;
  resultsByIssue = groupIssueResults(data);
  byId("repository").textContent = data.Repository;
  byId("last-updated").textContent = "更新 " + timeText(data.Updated) + (data.ViewingOnly ? " ・ダッシュボードのみ起動中" : "");
  message("issues-message", [data.Errors.issues, data.Errors.results, data.Errors.logs].filter(Boolean).join(" "));
  message("prs-message", data.Errors.prs);
  const issues = data.Issues || [];
  const prs = data.PRs || [];
  byId("metric-open").textContent = data.Errors.issues ? "—" : issues.filter((issue) => issue.Status !== "Done").length;
  byId("metric-ready").textContent = data.Errors.issues ? "—" : issues.filter((issue) => issue.Status === "Ready").length;
  byId("metric-blocked").textContent = data.Errors.issues ? "—" : issues.filter((issue) => issue.Status === "Blocked").length;
  byId("metric-prs").textContent = data.Errors.prs ? "—" : prs.length;
  renderIssues();
  fillTable("prs", prs.map((pr) => [
    itemButton("pr", pr), badge(pr.IsDraft ? "Draft" : pr.State), badge(pr.Checks),
    badge(pr.ReviewDecision), timeText(pr.UpdatedAt)
  ]), 5, "オープンな Pull Request はありません");
  renderItemDetail();
}

async function refresh() {
  const button = byId("refresh");
  if (button.disabled) return;
  button.disabled = true;
  try {
    const response = await fetch("/api/snapshot", { cache: "no-store" });
    if (!response.ok) throw new Error("dashboard unavailable");
    renderDashboard(await response.json());
  } catch {
    byId("last-updated").textContent = "更新に失敗しました。Poller の起動状態を確認してください。";
  } finally {
    button.disabled = false;
  }
}

byId("refresh").addEventListener("click", refresh);
byId("settings-open").addEventListener("click", openSettings);
byId("settings-close").addEventListener("click", () => byId("settings-dialog").close());
byId("settings-dialog").addEventListener("cancel", (event) => {
  if (savingSettings) event.preventDefault();
});
byId("settings-form").addEventListener("submit", saveSettings);
byId("search").addEventListener("input", renderIssues);
byId("status-filter").addEventListener("change", renderIssues);
byId("item-close").addEventListener("click", () => byId("item-dialog").close());
byId("item-refresh").addEventListener("click", loadDiscussion);
byId("item-approve").addEventListener("click", submitApproval);
byId("item-retry").addEventListener("click", retryImplementation);
byId("item-dialog").addEventListener("close", () => {
  selectedItem = undefined;
  approval = undefined;
  discussionRequest++;
});
byId("log-close").addEventListener("click", () => byId("log-dialog").close());
byId("feedback-close").addEventListener("click", () => byId("feedback-dialog").close());
refresh();
window.setInterval(() => {
  if (!document.hidden) refresh();
}, 30000);
