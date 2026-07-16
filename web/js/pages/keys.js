/**
 * API 密钥管理页面 - 组合式函数
 * 支持模态框添加和修改密钥设置，以及 Go 限额查询
 */

import { validateRequired } from "../api.js?v=20260619a";
const { ref, reactive } = Vue;

export function useKeys(api, showToast, t, showConfirm) {
  const keys = ref([]);
  const showKeyModal = ref(false);
  const editingKeyId = ref(null);
  const githubImportLoading = ref(false);
  const quotaLoading = ref({});
  const quotaData = ref({});
  const quotaResetAt = ref({});
  const quotaTick = ref(Date.now());
  let quotaTimer = null;
  let quotaRefreshTimer = null;
  const quotaRefreshIntervalMs = 5 * 60 * 1000;

  const newKey = reactive({
    value: "",
    label: "",
    weight: 1,
    proxy_url: "",
    cookie: "",
    workspace_id: "",
  });
  const githubImport = reactive({
    username: "",
    password: "",
    otp: "",
    totp_secret: "",
  });

  function openKeyModal() {
    editingKeyId.value = null;
    resetGithubImport();
    newKey.value = "";
    newKey.label = "";
    newKey.weight = 1;
    newKey.proxy_url = "";
    newKey.cookie = "";
    newKey.workspace_id = "";
    showKeyModal.value = true;
  }

  function openGithubImportModal() {
    openKeyModal();
  }

  function openKeySettings(key) {
    editingKeyId.value = key.id;
    newKey.value = "";
    newKey.label = key.label || "";
    newKey.weight = key.weight || 1;
    newKey.proxy_url = key.proxy_url || "";
    newKey.cookie = key.cookie || "";
    newKey.workspace_id = key.workspace_id || "";
    showKeyModal.value = true;
  }

  function closeKeyModal() {
    showKeyModal.value = false;
    editingKeyId.value = null;
    resetGithubImport();
  }

  function resetGithubImport() {
    githubImport.username = "";
    githubImport.password = "";
    githubImport.otp = "";
    githubImport.totp_secret = "";
    githubImportLoading.value = false;
  }

  function normalizeCookieInput(raw) {
    let value = String(raw || "").trim();
    if (!value) return "";
    value = value
      .replace(/^cookie:\s*/i, "")
      .replace(/^set-cookie:\s*/i, "")
      .trim();
    const match = value.match(/(?:^|[;\s])auth=([^;\s]+)/i);
    if (match && match[1]) return "auth=" + match[1].trim();
    if (!value.includes("=")) return "auth=" + value;
    return value;
  }

  function normalizeKeyCookie() {
    newKey.cookie = normalizeCookieInput(newKey.cookie);
  }

  function normalizeWorkspaceInput(raw) {
    const value = String(raw || "").trim();
    if (!value) return "";
    const match = value.match(/\b(wrk_[A-Za-z0-9][A-Za-z0-9_-]{5,127})\b/i);
    return match && match[1] ? match[1] : value;
  }

  function normalizeKeyWorkspace() {
    newKey.workspace_id = normalizeWorkspaceInput(newKey.workspace_id);
  }

  let currentGroup = "go";

  async function load(group) {
    if (group) currentGroup = group;
    try {
      const d = await api("/keys?group=" + encodeURIComponent(currentGroup), "GET", null, t);
      const rows = d.data || [];
      keys.value = rows;
      const nextQuota = {};
      const nextResetAt = {};
      for (const key of rows) {
        if (key.last_quota) {
          nextQuota[key.id] = key.last_quota;
          nextResetAt[key.id] = quotaResetTimes(key.last_quota);
        }
      }
      quotaData.value = nextQuota;
      quotaResetAt.value = nextResetAt;
      if (Object.keys(nextQuota).length || hasQuotaConfiguredKeys()) startQuotaTicker();
      if (hasQuotaConfiguredKeys()) {
        startQuotaAutoRefresh();
        refreshConfiguredQuotas({ silent: true });
      } else {
        stopQuotaAutoRefresh();
      }
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function add() {
    const editing = Boolean(editingKeyId.value);
    if (!editing && !validateRequired(newKey.value, t("keys.keyValue"), t, showToast))
      return;
    try {
      const payload = {
        value: newKey.value,
        group: currentGroup,
        label: newKey.label,
        weight: newKey.weight || 1,
        proxy_url: newKey.proxy_url,
      };
      // Only send cookie/workspace for Go group (Ollama Cloud doesn't need them)
      if (currentGroup === 'go') {
        payload.cookie = normalizeCookieInput(newKey.cookie);
        payload.workspace_id = normalizeWorkspaceInput(newKey.workspace_id);
      }
      if (editing) {
        await api("/keys/" + editingKeyId.value, "PATCH", payload, t);
        showToast(t("keys.updateBtn") + " ✓");
      } else {
        await api("/keys", "POST", payload, t);
        showToast(t("keys.addBtn") + " ✓");
      }
      closeKeyModal();
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function importGithubKey() {
    if (!validateRequired(githubImport.username, t("keys.githubLogin"), t, showToast)) return;
    if (!validateRequired(githubImport.password, t("keys.githubPassword"), t, showToast)) return;
    githubImportLoading.value = true;
    try {
      const d = await api(
        "/keys/import-github",
        "POST",
        {
          username: githubImport.username,
          password: githubImport.password,
          otp: githubImport.otp,
          totp_secret: githubImport.totp_secret,
          label: newKey.label,
          weight: newKey.weight || 1,
          proxy_url: newKey.proxy_url,
        },
        t
      );
      showToast(t("keys.githubImportSuccess", { count: d.imported || 0 }));
      closeKeyModal();
      load();
    } catch (e) {
      showToast(e.message, "error");
    } finally {
      githubImportLoading.value = false;
    }
  }

  async function toggle(id) {
    try {
      await api("/keys/" + id + "/toggle", "POST", null, t);
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function resetCooldown(id) {
    try {
      await api("/keys/" + id + "/reset", "POST", null, t);
      showToast(t("keys.cooldownReset") + " ✓");
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function fetchQuota(id, options = {}) {
    if (quotaLoading.value[id]) return quotaData.value[id];
    const silent = Boolean(options.silent);
    quotaLoading.value[id] = true;
    try {
      const d = await api("/keys/" + id + "/quota", "GET", null, t);
      quotaData.value[id] = d;
      quotaResetAt.value[id] = quotaResetTimes(d);
      startQuotaTicker();
      const key = keys.value.find((item) => item.id === id);
      if (key) {
        if (d?.workspaceID) key.workspace_id = d.workspaceID;
        if (d?.checkedAt) key.quota_updated_at = d.checkedAt;
        key.last_quota = d;
      }
      if (!silent && d && d.error) {
        showToast(d.hint || d.error, "error");
      }
      return d;
    } catch (e) {
      quotaData.value[id] = { error: e.message, configured: false };
      if (!silent) showToast(e.message, "error");
      return quotaData.value[id];
    } finally {
      quotaLoading.value[id] = false;
    }
  }

  function hasQuotaConfiguredKeys() {
    return keys.value.some((key) => quotaKeyCanRefresh(key));
  }

  function quotaKeyCanRefresh(key) {
    return Boolean(String(key?.cookie || "").trim());
  }

  function quotaRefreshKeyIds() {
    return keys.value.filter(quotaKeyCanRefresh).map((key) => key.id);
  }

  async function refreshConfiguredQuotas(options = {}) {
    const ids = quotaRefreshKeyIds();
    if (!ids.length) {
      stopQuotaAutoRefresh();
      return;
    }
    await Promise.all(ids.map((id) => fetchQuota(id, { silent: options.silent !== false })));
  }

  function startQuotaAutoRefresh() {
    if (quotaRefreshTimer || !hasQuotaConfiguredKeys()) return;
    quotaRefreshTimer = setInterval(() => {
      refreshConfiguredQuotas({ silent: true });
    }, quotaRefreshIntervalMs);
  }

  function stopQuotaAutoRefresh() {
    if (!quotaRefreshTimer) return;
    clearInterval(quotaRefreshTimer);
    quotaRefreshTimer = null;
  }

  async function useQuotaWorkspaceCandidate(id, candidate) {
    const workspaceID =
      typeof candidate === "string" ? candidate : String(candidate?.id || "").trim();
    if (!workspaceID) return;

    const key = keys.value.find((item) => item.id === id);
    if (!key) {
      showToast("未找到对应密钥", "error");
      return;
    }

    try {
      await api(
        "/keys/" + id,
        "PATCH",
        {
          label: key.label || "",
          weight: key.weight || 1,
          proxy_url: key.proxy_url || "",
          cookie: key.cookie || "",
          workspace_id: workspaceID,
          enabled: Boolean(key.enabled),
        },
        t
      );
      key.workspace_id = workspaceID;
      quotaData.value[id] = null;
      showToast("Workspace ID 已保存，正在重新查询 ✓");
      await fetchQuota(id);
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function remove(id) {
    const item = keys.value.find((k) => k.id === id);
    const name = item ? item.label || item.value : "";
    showConfirm(
      "deleteKey",
      async () => {
        try {
          await api("/keys/" + id, "DELETE", null, t);
          showToast(t("keys.delete") + " ✓");
          load();
        } catch (e) {
          showToast(e.message, "error");
        }
      },
      name
    );
  }

  // Format quota percent display
  function quotaPercent(percent) {
    if (percent === null || percent === undefined) return "—";
    return percent + "%";
  }

  function quotaBadgeClass(percent) {
    if (percent === null || percent === undefined || percent === "") return "badge";
    if (percent >= 80) return "badge-red";
    if (percent >= 50) return "badge-yellow";
    return "badge-green";
  }

  function quotaBuckets(data, keyId) {
    quotaTick.value;
    const quota = data?.quota || {};
    const usage = data?.usage || {};
    const resets = quotaResetAt.value[keyId] || {};
    return [
      { key: "total", label: t("keys.quotaTotal"), usage: usage.total || {} },
      { key: "rolling", label: t("keys.quotaRolling"), ...(quota.rolling || {}), usage: usage.rolling || {}, resetAt: resets.rolling },
      { key: "weekly", label: t("keys.quotaWeekly"), ...(quota.weekly || {}), usage: usage.weekly || {}, resetAt: resets.weekly },
      { key: "monthly", label: t("keys.quotaMonthly"), ...(quota.monthly || {}), usage: usage.monthly || {}, resetAt: resets.monthly },
    ];
  }

  function quotaResetLabel(bucket) {
    quotaTick.value;
    if (!bucket || bucket.key === "total") return "";
    const resetAt = Number(bucket.resetAt || 0);
    if (!Number.isFinite(resetAt) || resetAt <= 0) return t("keys.quotaResetUnknown");
    const remaining = Math.max(0, Math.floor((resetAt - Date.now()) / 1000));
    const resetAtLabel = new Date(resetAt).toLocaleString([], {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
    });
    return (
      t("keys.quotaResetAt", { time: resetAtLabel }) +
      "（" +
      t("keys.quotaCountdown", { time: formatQuotaReset(remaining) }) +
      "）"
    );
  }

  function quotaCheckedLabel(data) {
    const raw = data?.checkedAt || data?.checked_at;
    if (!raw) return "";
    const date = new Date(raw);
    if (Number.isNaN(date.getTime())) return "";
    return t("keys.quotaCheckedAt", { time: date.toLocaleString() });
  }

  function formatQuotaReset(seconds) {
    const n = Number(seconds);
    if (!Number.isFinite(n) || n < 0) return "—";
    const days = Math.floor(n / 86400);
    const hours = Math.floor((n % 86400) / 3600);
    const minutes = Math.floor((n % 3600) / 60);
    const parts = [];
    if (days) parts.push(days + " " + t("keys.quotaDay"));
    if (hours) parts.push(hours + " " + t("keys.quotaHour"));
    if (minutes || parts.length === 0) parts.push(minutes + " " + t("keys.quotaMinute"));
    return parts.slice(0, 2).join(" ");
  }

  function quotaUsageLabel(bucket) {
    const usage = bucket?.usage || {};
    const requests = formatQuotaNumber(usage.requests);
    const tokens = formatQuotaNumber(usage.totalTokens ?? usage.total_tokens);
    return `${requests} ${t("keys.quotaRequests")} · ${tokens} ${t("keys.quotaTokens")}`;
  }

  function formatQuotaNumber(value) {
    const n = Number(value || 0);
    if (!Number.isFinite(n) || n <= 0) return "0";
    return n.toLocaleString();
  }

  function quotaResetTimes(data) {
    const checkedAt = new Date(data?.checkedAt || data?.checked_at || Date.now()).getTime();
    const quota = data?.quota || {};
    const out = {};
    for (const key of ["rolling", "weekly", "monthly"]) {
      const sec = Number(quota[key]?.resetInSec);
      if (Number.isFinite(checkedAt) && Number.isFinite(sec) && sec > 0) {
        out[key] = checkedAt + sec * 1000;
      }
    }
    return out;
  }

  function startQuotaTicker() {
    if (quotaTimer) return;
    quotaTimer = setInterval(() => {
      quotaTick.value = Date.now();
    }, 60000);
  }

  function stopQuotaTicker() {
    if (quotaTimer) {
      clearInterval(quotaTimer);
      quotaTimer = null;
    }
    stopQuotaAutoRefresh();
  }

  function quotaWorkspaceCandidates(data) {
    if (!data || !Array.isArray(data.workspaceCandidates)) return [];
    return data.workspaceCandidates.filter((item) =>
      typeof item === "string" ? item.trim() : item && item.id
    );
  }

  function quotaCandidateLabel(candidate) {
    if (typeof candidate === "string") return candidate;
    const id = String(candidate?.id || "");
    const name = String(candidate?.name || "").trim();
    return name ? name + " (" + id + ")" : id;
  }

  return {
    keys,
    currentGroup,
    newKey,
    githubImport,
    githubImportLoading,
    editingKeyId,
    showKeyModal,
    quotaLoading,
    quotaData,
    quotaResetAt,
    openKeyModal,
    openGithubImportModal,
    openKeySettings,
    closeKeyModal,
    load,
    add,
    importGithubKey,
    toggle,
    resetCooldown,
    fetchQuota,
    refreshConfiguredQuotas,
    startQuotaAutoRefresh,
    stopQuotaAutoRefresh,
    useQuotaWorkspaceCandidate,
    remove,
    quotaPercent,
    quotaBadgeClass,
    quotaBuckets,
    quotaResetLabel,
    quotaUsageLabel,
    quotaCheckedLabel,
    quotaWorkspaceCandidates,
    quotaCandidateLabel,
    normalizeCookieInput,
    normalizeKeyCookie,
    normalizeWorkspaceInput,
    normalizeKeyWorkspace,
    startQuotaTicker,
    stopQuotaTicker,
  };
}
