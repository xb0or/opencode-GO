/**
 * 访问令牌管理页面 - 组合式函数
 * 支持模态框创建、复选框选择允许分组
 */

import { validateRequired } from "../api.js";
const { ref, reactive } = Vue;

export function useTokens(api, showToast, t, showConfirm) {
  const tokens = ref([]);
  const showTokenModal = ref(false);

  const newToken = reactive({
    name: "",
    allowed_groups: [],
    rate_limit: 0,
  });

  /** 打开模态框 */
  function openTokenModal() {
    newToken.name = "";
    newToken.allowed_groups = [];
    newToken.rate_limit = 0;
    showTokenModal.value = true;
  }

  /** 关闭模态框 */
  function closeTokenModal() {
    showTokenModal.value = false;
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
    if (!validateRequired(newToken.name, t("tokens.name"), t, showToast))
      return;
    try {
      // 将分组数组转为逗号分隔字符串发送给后端
      const payload = {
        name: newToken.name,
        allowed_groups: "",
        rate_limit: newToken.rate_limit,
      };
      await api("/tokens", "POST", payload, t);
      showToast(t("tokens.create") + " ✓");
      closeTokenModal();
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

  return {
    tokens,
    newToken,
    showTokenModal,
    openTokenModal,
    closeTokenModal,
    load,
    add,
    remove,
  };
}
