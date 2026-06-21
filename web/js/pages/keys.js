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
  const quotaLoading = ref({});
  const quotaData = ref({});

  const newKey = reactive({
    value: "",
    label: "",
    weight: 1,
    proxy_url: "",
    cookie: "",
    workspace_id: "",
  });

  function openKeyModal() {
    editingKeyId.value = null;
    newKey.value = "";
    newKey.label = "";
    newKey.weight = 1;
    newKey.proxy_url = "";
    newKey.cookie = "";
    newKey.workspace_id = "";
    showKeyModal.value = true;
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

  async function load() {
    try {
      const d = await api("/keys", "GET", null, t);
      const rows = d.data || [];
      keys.value = rows;
      const nextQuota = {};
      for (const key of rows) {
        if (key.last_quota) nextQuota[key.id] = key.last_quota;
      }
      quotaData.value = nextQuota;
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
        label: newKey.label,
        weight: newKey.weight || 1,
        proxy_url: newKey.proxy_url,
        cookie: normalizeCookieInput(newKey.cookie),
        workspace_id: normalizeWorkspaceInput(newKey.workspace_id),
      };
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

  async function fetchQuota(id) {
    quotaLoading.value[id] = true;
    try {
      const d = await api("/keys/" + id + "/quota", "GET", null, t);
      quotaData.value[id] = d;
      const key = keys.value.find((item) => item.id === id);
      if (key) {
        if (d?.workspaceID) key.workspace_id = d.workspaceID;
        if (d?.checkedAt) key.quota_updated_at = d.checkedAt;
        key.last_quota = d;
      }
      if (d && d.error) {
        showToast(d.hint || d.error, "error");
      }
    } catch (e) {
      quotaData.value[id] = { error: e.message, configured: false };
      showToast(e.message, "error");
    } finally {
      quotaLoading.value[id] = false;
    }
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

  function quotaBuckets(data) {
    const quota = data?.quota || {};
    return [
      { key: "rolling", label: t("keys.quotaRolling"), ...(quota.rolling || {}) },
      { key: "weekly", label: t("keys.quotaWeekly"), ...(quota.weekly || {}) },
      { key: "monthly", label: t("keys.quotaMonthly"), ...(quota.monthly || {}) },
    ];
  }

  function quotaResetLabel(bucket) {
    if (!bucket || (!bucket.resetIn && bucket.resetInSec !== 0)) return t("keys.quotaResetUnknown");
    const resetIn =
      bucket.resetInSec !== null && bucket.resetInSec !== undefined
        ? formatQuotaReset(bucket.resetInSec)
        : bucket.resetIn;
    return t("keys.quotaResetIn", { time: resetIn });
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
    newKey,
    editingKeyId,
    showKeyModal,
    quotaLoading,
    quotaData,
    openKeyModal,
    openKeySettings,
    closeKeyModal,
    load,
    add,
    toggle,
    resetCooldown,
    fetchQuota,
    useQuotaWorkspaceCandidate,
    remove,
    quotaPercent,
    quotaBadgeClass,
    quotaBuckets,
    quotaResetLabel,
    quotaCheckedLabel,
    quotaWorkspaceCandidates,
    quotaCandidateLabel,
    normalizeCookieInput,
    normalizeKeyCookie,
    normalizeWorkspaceInput,
    normalizeKeyWorkspace,
  };
}
