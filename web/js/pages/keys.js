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

  async function load() {
    try {
      const d = await api("/keys", "GET", null, t);
      keys.value = d.data || [];
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
        cookie: newKey.cookie,
        workspace_id: newKey.workspace_id,
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
    } catch (e) {
      quotaData.value[id] = { error: e.message, configured: false };
      showToast(e.message, "error");
    } finally {
      quotaLoading.value[id] = false;
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
    if (percent >= 80) return "badge-red";
    if (percent >= 50) return "badge-yellow";
    return "badge-green";
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
    remove,
    quotaPercent,
    quotaBadgeClass,
  };
}