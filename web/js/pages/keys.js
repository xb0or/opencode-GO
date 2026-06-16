/**
 * API 密钥管理页面 - 组合式函数
 * 支持模态框添加密钥
 */

import { validateRequired } from "../api.js";
const { ref, reactive } = Vue;

export function useKeys(api, showToast, t, showConfirm) {
  const keys = ref([]);
  const showKeyModal = ref(false);

  const newKey = reactive({
    value: "",
    group: "go",
    label: "",
    weight: 1,
    proxy_url: "",
  });

  const keyGroupOptions = ["go"];

  function openKeyModal() {
    newKey.value = "";
    newKey.group = "go";
    newKey.label = "";
    newKey.weight = 1;
    newKey.proxy_url = "";
    showKeyModal.value = true;
  }

  function closeKeyModal() {
    showKeyModal.value = false;
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
    if (!validateRequired(newKey.value, t("keys.keyValue"), t, showToast))
      return;
    try {
      await api("/keys", "POST", newKey, t);
      showToast(t("keys.addBtn") + " ✓");
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
    showKeyModal,
    keyGroupOptions,
    openKeyModal,
    closeKeyModal,
    load,
    add,
    toggle,
    resetCooldown,
    remove,
  };
}
