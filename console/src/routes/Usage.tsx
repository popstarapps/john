import { useState } from "react";
import { getUsage } from "../api/client";
import { useQuery } from "../lib/useQuery";
import { Loading, PageHead, Segmented } from "../components/ui";
import { ByModel, SpendPerDay, UsageTiles } from "../components/usage";

type Range = "7" | "30" | "60";

/** The console's home. What John cost you, and what it ran on. */
export default function Usage() {
  const [range, setRange] = useState<Range>("30");
  const { data } = useQuery(() => getUsage({ days: Number(range), scope: "me" }), [range]);

  if (!data) return <Loading rows={6} height={70} />;

  return (
    <>
      <PageHead
        title="Usage"
        lede={`Your usage the last ${range} days.`}
        actions={
          <Segmented
            value={range}
            onChange={setRange}
            label="Time range"
            options={[
              { value: "7", label: "7d" },
              { value: "30", label: "30d" },
              { value: "60", label: "60d" },
            ]}
          />
        }
      />

      <div className="stack">
        <UsageTiles summary={data} days={range} />
        <SpendPerDay summary={data} days={range} />
        <ByModel summary={data} />
      </div>
    </>
  );
}
