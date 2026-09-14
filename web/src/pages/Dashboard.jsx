import React from 'react';
import { Link } from 'react-router-dom';
import { Card } from '../components.jsx';
import { FEATURES } from '../features.js';

// Dashboard is a way in, not a report.
//
// The numbers it used to restate all live on the pages that own them, and a
// screen that only repeats them is a click in the way. What is useful on the
// first screen is knowing what the panel can do, which on a control panel is
// not obvious from a sidebar of eight words.
export default function Dashboard({ me }) {
  const role = me.role;
  const allowed = (item) => !item.roles || item.roles.includes(role);

  return (
    <>
      {FEATURES.filter((g) => !g.roles || g.roles.includes(role)).map((group) => {
        const items = group.items.filter(allowed);
        if (items.length === 0) return null;
        return (
          <Card key={group.group} title={group.group}>
            <div className="tiles">
              {items.map((item) => (
                <Link key={item.to} className="tile" to={item.to}>
                  <strong>{item.label}</strong>
                  <span>{item.desc}</span>
                </Link>
              ))}
            </div>
          </Card>
        );
      })}
    </>
  );
}
