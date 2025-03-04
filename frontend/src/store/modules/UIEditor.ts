import axios from "axios";
import { defineStore } from "pinia";
import { cloneDeep, isUndefined, uniqueId } from "lodash-es";
import {
  DatabaseId,
  Database,
  TabContext,
  UIEditorState,
  UIEditorTabType,
  TableTabContext,
  DatabaseEdit,
} from "@/types";
import { Table } from "@/types/UIEditor";
import { useDatabaseStore, useTableStore } from "./";
import { transformTableDataToTable } from "@/utils/UIEditor/transform";

export const generateUniqueTabId = () => {
  return uniqueId();
};

const getDefaultUIEditorState = (): UIEditorState => {
  return {
    tabState: {
      tabMap: new Map<string, TabContext>(),
      currentTabId: "",
    },
    databaseList: [],
    originTableList: [],
    tableList: [],
  };
};

export const useUIEditorStore = defineStore("UIEditor", {
  state: (): UIEditorState => {
    return getDefaultUIEditorState();
  },
  getters: {
    currentTab(state) {
      if (isUndefined(state.tabState.currentTabId)) {
        return undefined;
      }
      return state.tabState.tabMap.get(state.tabState.currentTabId);
    },
    tabList(state) {
      return Array.from(state.tabState.tabMap.values());
    },
  },
  actions: {
    addTab(tab: TabContext, setAsCurrentTab = true) {
      const tabCache = this.tabList.find((item) => {
        if (item.type !== tab.type) {
          return false;
        }

        if (
          item.type === UIEditorTabType.TabForDatabase &&
          item.databaseId === tab.databaseId
        ) {
          return true;
        }
        if (
          item.type === UIEditorTabType.TabForTable &&
          item.databaseId === tab.databaseId &&
          item.tableName === (tab as TableTabContext).tableName
        ) {
          return true;
        }
        return false;
      });

      if (tabCache !== undefined) {
        tab = tabCache;
      } else {
        this.tabState.tabMap.set(tab.id, tab);
      }

      if (setAsCurrentTab) {
        this.setCurrentTab(tab.id);
      }
    },
    saveTab(tab: TabContext) {
      if (tab.type === UIEditorTabType.TabForDatabase) {
        // Edit database metadata is not allowed.
      } else if (tab.type === UIEditorTabType.TabForTable) {
        // tab.table.name = tab.tableCache.name;
        // tab.table.columnList = cloneDeep(tab.tableCache.columnList);
      }
    },
    setCurrentTab(tabId: string) {
      if (isUndefined(this.tabState.tabMap.get(tabId))) {
        this.tabState.currentTabId = undefined;
      } else {
        this.tabState.currentTabId = tabId;
      }
    },
    closeTab(tabId: string) {
      const tabList = Array.from(this.tabState.tabMap.values());
      const tabIndex = tabList.findIndex((tab) => tab.id === tabId);
      // Find next tab for showing.
      if (this.tabState.currentTabId === tabId) {
        let nextTabIndex = -1;
        if (tabIndex === 0) {
          nextTabIndex = 1;
        } else {
          nextTabIndex = tabIndex - 1;
        }
        const nextTab = tabList[nextTabIndex];
        if (nextTab) {
          this.setCurrentTab(nextTab.id);
        } else {
          this.setCurrentTab("");
        }
      }
      this.tabState.tabMap.delete(tabId);
    },
    findTab(databaseId: DatabaseId, tableName?: string) {
      let tabType = UIEditorTabType.TabForDatabase;
      if (tableName !== undefined) {
        tabType = UIEditorTabType.TabForTable;
      }

      const tab = this.tabList.find((tab) => {
        if (tab.type !== tabType || tab.databaseId !== databaseId) {
          return false;
        }

        if (tab.type === UIEditorTabType.TabForDatabase) {
          return true;
        } else if (
          tab.type === UIEditorTabType.TabForTable &&
          tab.tableName === tableName
        ) {
          return true;
        }

        return false;
      });

      return tab;
    },
    async fetchDatabaseList(databaseIdList: DatabaseId[]) {
      const databaseList: Database[] = [];
      for (const id of databaseIdList) {
        const database = cloneDeep(
          await useDatabaseStore().getOrFetchDatabaseById(id)
        );
        databaseList.push(database);
      }
      this.databaseList = databaseList;
      return databaseList;
    },
    async getOrFetchTableListByDatabaseId(databaseId: DatabaseId) {
      const tableList: Table[] = [];
      for (const table of this.tableList) {
        if (table.databaseId === databaseId) {
          tableList.push(table);
        }
      }

      if (tableList.length === 0) {
        const tableDataList = await useTableStore().fetchTableListByDatabaseId(
          databaseId
        );
        const transformTableList = tableDataList.map((tableData) =>
          transformTableDataToTable(tableData)
        );

        tableList.push(...transformTableList);
        this.originTableList.push(...transformTableList);
        this.tableList.push(...cloneDeep(transformTableList));
      }
      return tableList;
    },
    getTableWithTableTab(tab: TableTabContext) {
      return this.tableList.find(
        (table) =>
          table.databaseId === tab.databaseId && table.newName === tab.tableName
      );
    },
    dropTable(table: Table) {
      const index = this.tableList.findIndex((item) => item === table);
      if (table.status === "created") {
        this.tableList.splice(index, 1);
        // Close tab for new table.
        const tab = this.findTab(table.databaseId, table.newName);
        if (tab) {
          this.closeTab(tab.id);
        }
      } else {
        table.status = "dropped";
      }
    },
    restoreTable(table: Table) {
      delete table.status;
    },
    async postDatabaseEdit(databaseEdit: DatabaseEdit) {
      const stmt = (
        await axios.post<string>(
          `/api/database/${databaseEdit.databaseId}/edit`,
          databaseEdit
        )
      ).data;
      return stmt;
    },
  },
});
