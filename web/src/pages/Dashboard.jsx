import React from 'react';
import { Link } from 'react-router-dom';
import { Card } from '../components.jsx';
import { FEATURES } from '../features.js';
import Icon from '../icons.jsx';

// Dashboard is a way in, not a report.
//
// The numbers it used to restate all live on the pages that own them, and a
// screen that only repeats them is a click in the way. What is left is the
// shortest description of a control panel there is: what it can do.
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
                  <Icon name={item.icon} size={26} />
                  <span>{item.label}</span>
                </Link>
              ))}
            </div>
          </Card>
        );
      })}
    </>
  );
}
