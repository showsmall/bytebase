import { Database, DatabaseId, TableEngineType, TableType } from ".";

type TableOrColumnStatus = "created" | "dropped";

export interface Column {
  // Related fields
  databaseId: DatabaseId;

  // Domain specific fields
  oldName: string;
  newName: string;
  type: string;
  nullable: boolean;
  comment: string;
  default: string | null;

  status?: TableOrColumnStatus;
}

export interface Table {
  // Related fields
  databaseId: DatabaseId;

  // Domain specific fields
  oldName: string;
  newName: string;
  type: TableType;
  engine: TableEngineType;
  collation: string;
  rowCount: number;
  dataSize: number;
  comment: string;
  columnList: Column[];
  originColumnList: Column[];

  status?: TableOrColumnStatus;
}

export enum UIEditorTabType {
  TabForDatabase = "database",
  TabForTable = "table",
}

// Tab context for editing database.
export interface DatabaseTabContext {
  id: string;
  type: UIEditorTabType.TabForDatabase;
  databaseId: DatabaseId;
}

// Tab context for editing table.
export interface TableTabContext {
  id: string;
  type: UIEditorTabType.TabForTable;
  databaseId: DatabaseId;
  tableName: string;
}

export type TabContext = DatabaseTabContext | TableTabContext;

type TabId = string;

export interface UIEditorState {
  tabState: {
    tabMap: Map<TabId, TabContext>;
    currentTabId?: TabId;
  };
  databaseList: Database[];
  originTableList: Table[];
  tableList: Table[];
}

/**
 * Type definition for API message.
 */
export interface DatabaseEdit {
  databaseId: DatabaseId;

  createTableList: CreateTableContext[];
  alterTableList: AlterTableContext[];
  renameTableList: RenameTableContext[];
  dropTableList: DropTableContext[];
}

export interface CreateTableContext {
  name: string;
  type: string;
  engine: string;
  characterSet: string;
  collation: string;
  comment: string;

  addColumnList: AddColumnContext[];
}

export interface AlterTableContext {
  name: string;

  addColumnList: AddColumnContext[];
  changeColumnList: ChangeColumnContext[];
  dropColumnList: DropColumnContext[];
}

export interface RenameTableContext {
  oldName: string;
  newName: string;
}

export interface DropTableContext {
  name: string;
}

export interface AddColumnContext {
  name: string;
  type: string;
  characterSet: string;
  collation: string;
  comment: string;
  nullable: boolean;
  default?: string;
}

export interface ChangeColumnContext {
  oldName: string;
  newName: string;
  type: string;
  characterSet: string;
  collation: string;
  comment: string;
  nullable: boolean;
  default?: string;
}

export interface DropColumnContext {
  name: string;
}
