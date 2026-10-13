import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "./components/AppShell";
import { ToastHost } from "./components/overlays";

import Usage from "./routes/Usage";
import Requests from "./routes/Requests";
import RequestNew from "./routes/RequestNew";
import RequestDetail from "./routes/RequestDetail";
import MyPolicies from "./routes/MyPolicies";
import Download from "./routes/Download";
import NotFound from "./routes/NotFound";
import SignIn from "./routes/SignIn";
import Setup from "./routes/Setup";

import AdminUsage from "./routes/admin/AdminUsage";
import AdminRequests from "./routes/admin/AdminRequests";
import Groups from "./routes/admin/Groups";
import GroupDetail from "./routes/admin/GroupDetail";
import AdminTeams from "./routes/admin/AdminTeams";
import TeamDetail from "./routes/admin/TeamDetail";
import Members from "./routes/admin/Members";
import MemberDetail from "./routes/admin/MemberDetail";
import Models from "./routes/admin/Models";
import Traces from "./routes/admin/Traces";
import History from "./routes/admin/History";
import Settings from "./routes/admin/Settings";

/**
 * Two layouts: the console shell, and the pages that deliberately have no
 * shell — sign-in and first-run setup, both of which exist before there is
 * anything to navigate to.
 *
 * The developer half is deliberately small: usage, the requests you have made,
 * the policies and teams you are subject to, and the download. Traces live
 * under /admin, where reviewing someone's activity is the job.
 */
export default function App() {
  return (
    <BrowserRouter>
      <ToastHost>
        <Routes>
          <Route path="/signin" element={<SignIn />} />
          <Route path="/setup" element={<Setup />} />

          <Route element={<AppShell />}>
            <Route index element={<Usage />} />
            <Route path="requests" element={<Requests />} />
            <Route path="requests/new" element={<RequestNew />} />
            <Route path="requests/:id" element={<RequestDetail />} />
            <Route path="policies" element={<MyPolicies />} />
            <Route path="download" element={<Download />} />

            <Route path="admin" element={<AdminUsage />} />
            <Route path="admin/requests" element={<AdminRequests />} />
            <Route path="admin/requests/:id" element={<RequestDetail />} />
            <Route path="admin/groups" element={<Groups />} />
            <Route path="admin/groups/:id" element={<GroupDetail />} />
            <Route path="admin/teams" element={<AdminTeams />} />
            <Route path="admin/teams/:slug" element={<TeamDetail />} />
            <Route path="admin/members" element={<Members />} />
            <Route path="admin/members/:id" element={<MemberDetail />} />
            <Route path="admin/models" element={<Models />} />
            <Route path="admin/traces" element={<Traces />} />
            <Route path="admin/history" element={<History />} />
            <Route path="admin/settings" element={<Settings />} />
            <Route path="admin/settings/:tab" element={<Settings />} />

            <Route path="404" element={<NotFound />} />
            <Route path="*" element={<Navigate to="/404" replace />} />
          </Route>
        </Routes>
      </ToastHost>
    </BrowserRouter>
  );
}
