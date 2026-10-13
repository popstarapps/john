import { useQuery } from "./useQuery";
import { listModels, listPolicyGroups, listTeams, listUsers } from "../api/client";
import type { ID, Model, PolicyGroup, Team, User } from "../api/types";

export interface Directory {
  users: User[];
  teams: Team[];
  models: Model[];
  groups: PolicyGroup[];
  user: (id: ID | null | undefined) => User | undefined;
  team: (id: ID | null | undefined) => Team | undefined;
  model: (id: ID | null | undefined) => Model | undefined;
  group: (id: ID | null | undefined) => PolicyGroup | undefined;
  /** Name for an id, or an em dash — the common case in tables. */
  userName: (id: ID | null | undefined) => string;
  teamName: (id: ID | null | undefined) => string;
  modelName: (id: ID | null | undefined) => string;
  groupName: (id: ID | null | undefined) => string;
}

const EMPTY: Directory = {
  users: [],
  teams: [],
  models: [],
  groups: [],
  user: () => undefined,
  team: () => undefined,
  model: () => undefined,
  group: () => undefined,
  userName: () => "—",
  teamName: () => "—",
  modelName: () => "—",
  groupName: () => "—",
};

/**
 * Names for ids. Nearly every list needs to turn a `userId` into a person, so
 * it is loaded once here rather than joined by hand in every view.
 */
export function useDirectory(): Directory {
  const { data } = useQuery(
    async () => {
      const [users, teams, models, groups] = await Promise.all([
        listUsers(),
        listTeams(),
        listModels(),
        listPolicyGroups(),
      ]);
      return { users, teams, models, groups };
    },
    [],
  );

  if (!data) return EMPTY;

  const find = <T extends { id: ID }>(xs: T[]) => (id: ID | null | undefined) =>
    id ? xs.find((x) => x.id === id) : undefined;

  const user = find(data.users);
  const team = find(data.teams);
  const model = find(data.models);
  const group = find(data.groups);

  return {
    ...data,
    user,
    team,
    model,
    group,
    userName: (id) => user(id)?.name ?? "—",
    teamName: (id) => team(id)?.name ?? "—",
    modelName: (id) => model(id)?.name ?? "—",
    groupName: (id) => group(id)?.name ?? "—",
  };
}
