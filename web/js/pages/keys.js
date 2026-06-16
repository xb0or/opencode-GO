/**
 * API 密钥管理页面 - 组合式函数
 * 支持模态框添加和修改密钥设置
 */

import { validateRequired } from "../api.js";
const { ref, reactive } = Vue;

export function useKeys(api, showToast, t, showConfirm) {
  const keys = ref([]);
  const showKeyModal = ref(false);
  const editingKeyId = ref(null);

  const newKey = reactive({
    value: "",
    label: "",
    weight: 1,
    proxy_url: "",
  });

  function openKeyModal() {
    editingKeyId.value = null;
    newKey.value = "";
    newKey.label = "";
    newKey.weight = 1;
    newKey.proxy_url = "";
    showKeyModal.value = true;
  }

  function openKeySettings(key) {
    editingKeyId.value = key.id;
    newKey.value = "";
    newKey.label = key.label || "";
    newKey.weight = key.weight || 1;
    newKey.proxy_url = key.proxy_url || "";
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

  return {
    keys,
    newKey,
    editingKeyId,
    showKeyModal,
    openKeyModal,
    openKeySettings,
    closeKeyModal,
    load,
    add,
    toggle,
    resetCooldown,
    remove,
  };
}
