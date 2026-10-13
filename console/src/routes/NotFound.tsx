import { Link } from "react-router-dom";
import { Card, Empty, PageHead } from "../components/ui";

export default function NotFound() {
  return (
    <>
      <PageHead title="Not found" />
      <Card>
        <Empty icon="search" title="There is nothing at this address">
          The page may have moved, or the record may never have existed.{" "}
          <Link to="/">Back to the overview</Link>
        </Empty>
      </Card>
    </>
  );
}
