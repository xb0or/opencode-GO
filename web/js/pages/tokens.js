/**
 * 访问令牌管理页面 - 组合式函数
 * 支持模态框创建、修改、删除，以及过期时间、请求限额和用量追踪
 */

const { ref, reactive } = Vue;

export function useTokens(api, showToast, t, showConfirm) {
  const tokens = ref([]);
  const showTokenModal = ref(false);
  const editingTokenId = ref(null);

  const newToken = reactive({
    name: "",
    description: "",
    rate_limit: 0,
    max_requests: 0,
    expires_at: "",
  });

  /** 打开创建模态框 */
  function openTokenModal() {
    editingTokenId.value = null;
    newToken.name = "";
    newToken.description = "";
    newToken.rate_limit = 0;
    newToken.max_requests = 0;
    newToken.expires_at = "";
    showTokenModal.value = true;
  }

  /** 打开编辑模态框 */
  function openTokenSettings(tk) {
    editingTokenId.value = tk.id;
    newToken.name = tk.name || "";
    newToken.description = tk.description || "";
    newToken.rate_limit = tk.rate_limit || 0;
    newToken.max_requests = tk.max_requests || 0;
    newToken.expires_at = tk.expires_at ? tk.expires_at.slice(0, 16) : "";
    showTokenModal.value = true;
  }

  /** 关闭模态框 */
  function closeTokenModal() {
    showTokenModal.value = false;
    editingTokenId.value = null;
  }

  async function load() {
    try {
      const d = await api("/tokens", "GET", null, t);
      tokens.value = d.data || [];
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function add() {
    if (!newToken.name && !editingTokenId.value) {
      showToast(t("tokens.name") + " " + t("errors.required"), "error");
      return;
    }
    try {
      const payload = {
        name: newToken.name,
        description: newToken.description,
        rate_limit: parseInt(newToken.rate_limit) || 0,
        max_requests: parseInt(newToken.max_requests) || 0,
      };
      if (newToken.expires_at) {
        payload.expires_at = new Date(newToken.expires_at).toISOString();
      }
      if (editingTokenId.value) {
        await api("/tokens/" + editingTokenId.value, "PATCH", payload, t);
        showToast(t("tokens.updateBtn") + " ✓");
      } else {
        await api("/tokens", "POST", payload, t);
        showToast(t("tokens.create") + " ✓");
      }
      closeTokenModal();
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function toggle(id, enabled) {
    try {
      await api("/tokens/" + id, "PATCH", { enabled: Boolean(enabled) }, t);
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function remove(id) {
    const item = tokens.value.find((tk) => tk.id === id);
    const name = item ? item.name || item.token : "";
    showConfirm(
      "deleteToken",
      async () => {
        try {
          await api("/tokens/" + id, "DELETE", null, t);
          showToast(t("tokens.delete") + " ✓");
          load();
        } catch (e) {
          showToast(e.message, "error");
        }
      },
      name
    );
  }

  /** 格式化为日期时间字符串 */
  function fmtTokenExpiry(iso) {
    if (!iso) return t("tokens.never");
    const d = new Date(iso);
    if (isNaN(d.getTime())) return t("tokens.never");
    if (d < new Date()) return t("tokens.expired");
    return d.toLocaleString();
  }

  function expiryBadgeClass(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    if (d < new Date()) return "badge-red";
    const days = (d - new Date()) / 86400000;
    if (days < 3) return "badge-yellow";
    return "badge-green";
  }

  function requestUsedLabel(tk) {
    if (!tk.max_requests) return t("tokens.unlimited");
    return tk.requests_used + " / " + tk.max_requests;
  }

  function requestUsedPercent(tk) {
    if (!tk.max_requests) return 0;
    return Math.round((tk.requests_used / tk.max_requests) * 100);
  }

  function requestBadgeClass(tk) {
    if (!tk.max_requests) return "";
    const pct = requestUsedPercent(tk);
    if (pct >= 90) return "badge-red";
    if (pct >= 70) return "badge-yellow";
    return "badge-green";
  }

  return {
    tokens,
    newToken,
    editingTokenId,
    showTokenModal,
    openTokenModal,
    openTokenSettings,
    closeTokenModal,
    load,
    add,
    toggle,
    remove,
    fmtTokenExpiry,
    expiryBadgeClass,
    requestUsedLabel,
    requestUsedPercent,
    requestBadgeClass,
  };
}