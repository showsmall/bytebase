import { isEqual, isUndefined, omit } from "lodash-es";
import type {
  AddColumnContext,
  DropColumnContext,
  ChangeColumnContext,
} from "@/types";
import { Column } from "@/types/UIEditor";
import {
  transformColumnToAddColumnContext,
  transformColumnToChangeColumnContext,
} from "./transform";

// diffColumnList gets the differences between column object list.
// Including addColumnList, modifyColumnList and dropColumnList.
export const diffColumnList = (
  originColumnList: Column[],
  columnList: Column[]
) => {
  const addColumnContextList: AddColumnContext[] = [];
  const addedColumnList = columnList.filter(
    (column) => column.status === "created"
  );
  for (const column of addedColumnList) {
    addColumnContextList.push(transformColumnToAddColumnContext(column));
  }

  const changeColumnContextList: ChangeColumnContext[] = [];
  const changedColumnList = columnList.filter(
    (column) => column.status === undefined
  );
  for (const column of changedColumnList) {
    const originColumn = originColumnList.find(
      (originColumn) => originColumn.oldName === column.oldName
    );
    if (isUndefined(originColumn)) {
      continue;
    }
    if (!isEqual(omit(originColumn, "status"), omit(column, "status"))) {
      changeColumnContextList.push(
        transformColumnToChangeColumnContext(originColumn, column)
      );
    }
  }

  const dropColumnContextList: DropColumnContext[] = [];
  const droppedColumnList = columnList.filter(
    (column) => column.status === "dropped"
  );
  for (const column of droppedColumnList) {
    dropColumnContextList.push({
      name: column.oldName,
    });
  }

  return {
    addColumnList: addColumnContextList,
    changeColumnList: changeColumnContextList,
    dropColumnList: dropColumnContextList,
  };
};
